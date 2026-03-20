package main

import (
	"bytes"
	"compress/flate"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"io"
	"syscall/js"
)

// ── Vector format constants ──────────────────────────────────────────────────
// Wire format (uncompressed payload, then FLATE level-9 compressed):
//   Header     : magic 'V' (1) | version 0x01 (1) | cmdCount uint16LE
//   CMD_STROKE (0x01): tag(1) | R G B W(4) | pointCount uint16LE
//                    | x0 uint16LE | y0 uint16LE  (first point, absolute)
//                    | dx int16LE  | dy int16LE   (repeated pointCount-1)
//   CMD_CLEAR  (0x02): tag(1)        - no payload
//   CMD_FILL   (0x03): tag(1) | R G B (3)
//
// Encryption envelope (replaces old "ENC:" string prefix):
//   Encrypted URL  : base64url( 0x45 'E' | AES-256-GCM(FLATE_payload) )
//   Unencrypted URL: base64url( FLATE_payload )   <- starts with 0x78 (FLATE)
// Detection: first raw byte == 0x45 -> encrypted; else -> try FLATE -> legacy.

const (
	vecMagic     = byte('V')
	vecVersion   = byte(0x01)
	vecTagStroke = byte(0x01)
	vecTagClear  = byte(0x02)
	vecTagFill   = byte(0x03)
	encMagic     = byte('E') // 0x45 - flags an encrypted payload
)

type vecCmd interface{ isVecCmd() }

type vecCmdStroke struct {
	r, g, b    byte
	width      byte
	pts        [][2]int16 // pts[0] = absolute (x,y); pts[1..] = int16 deltas
	absX, absY int
}

func (v *vecCmdStroke) isVecCmd() {}

type vecCmdClear struct{}

func (v vecCmdClear) isVecCmd() {}

type vecCmdFill struct{ r, g, b byte }

func (v vecCmdFill) isVecCmd() {}

var vecCmds []vecCmd           // full undo/redo history (all commands ever committed)
var historyPos int             // number of commands currently applied; undo/redo moves this
var vecCurStroke *vecCmdStroke // stroke currently being built (not yet committed)

// historyPush appends cmd at historyPos, discarding any redo-able commands ahead
// of the cursor (new action always clears the redo stack).
func historyPush(cmd vecCmd) {
	vecCmds = append(vecCmds[:historyPos], cmd)
	historyPos++
}

func vecStartStroke(x, y int) {
	vecEndStroke() // commit any open stroke before starting a new one
	w := penWidth
	if w > 255 {
		w = 255
	}
	if w < 1 {
		w = 1
	}
	vecCurStroke = &vecCmdStroke{
		r: penColor.R, g: penColor.G, b: penColor.B,
		width: byte(w), absX: x, absY: y,
	}
	vecCurStroke.pts = append(vecCurStroke.pts, [2]int16{int16(x), int16(y)})
}

func vecAddPoint(x, y int) {
	if vecCurStroke == nil {
		return
	}
	dx := x - vecCurStroke.absX
	dy := y - vecCurStroke.absY
	if dx > 32767 {
		dx = 32767
	}
	if dx < -32768 {
		dx = -32768
	}
	if dy > 32767 {
		dy = 32767
	}
	if dy < -32768 {
		dy = -32768
	}
	vecCurStroke.pts = append(vecCurStroke.pts, [2]int16{int16(dx), int16(dy)})
	vecCurStroke.absX, vecCurStroke.absY = x, y
}

func vecEndStroke() {
	if vecCurStroke == nil || len(vecCurStroke.pts) == 0 {
		vecCurStroke = nil
		return
	}
	historyPush(vecCurStroke)
	vecCurStroke = nil
}

var (
	canvas                    js.Value
	ctx                       js.Value
	drawing                   bool
	lastX, lastY              int
	penColor                  color.RGBA = color.RGBA{0, 0, 0, 255}
	penWidth                  int        = 2
	imgData                   *image.RGBA
	canvasWidth, canvasHeight int
)

func main() {
	// Parse canvas size from URL params
	canvasWidth, canvasHeight = 640, 480
	urlParams := js.Global().Get("URLSearchParams").New(js.Global().Get("location").Get("search"))
	if urlParams.Call("has", "w").Bool() {
		w := urlParams.Call("get", "w").String()
		if val := parseIntSafe(w, 640); val >= 64 && val <= 2048 {
			canvasWidth = val
		}
	}
	if urlParams.Call("has", "h").Bool() {
		h := urlParams.Call("get", "h").String()
		if val := parseIntSafe(h, 480); val >= 64 && val <= 2048 {
			canvasHeight = val
		}
	}

	imgData = image.NewRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))
	for i := range imgData.Pix {
		imgData.Pix[i] = 255
	}

	// Parse pen width from URL params
	if urlParams.Call("has", "pen").Bool() {
		p := urlParams.Call("get", "pen").String()
		if val := parseIntSafe(p, 2); val >= 1 && val <= 32 {
			penWidth = val
		}
	}

	doc := js.Global().Get("document")
	canvas = doc.Call("getElementById", "canvas")
	canvas.Set("width", canvasWidth)
	canvas.Set("height", canvasHeight)
	ctx = canvas.Call("getContext", "2d")

	ctx.Set("fillStyle", "white")
	ctx.Call("fillRect", 0, 0, canvasWidth, canvasHeight)

	loadFromURL()

	canvas.Call("addEventListener", "mousedown", js.FuncOf(mouseDown))
	canvas.Call("addEventListener", "mousemove", js.FuncOf(mouseMove))
	canvas.Call("addEventListener", "mouseup", js.FuncOf(mouseUp))
	canvas.Call("addEventListener", "mouseleave", js.FuncOf(mouseLeave))
	canvas.Call("addEventListener", "mouseenter", js.FuncOf(mouseEnter))
	canvas.Call("addEventListener", "contextmenu", js.FuncOf(contextMenu))
	// Also listen for mouseup on the document so releasing the button outside
	// the canvas boundary still commits the stroke and clears drawing state.
	doc.Call("addEventListener", "mouseup", js.FuncOf(mouseUp))

	js.Global().Set("setColor", js.FuncOf(setColor))
	js.Global().Set("setWidth", js.FuncOf(setWidth))
	js.Global().Set("exportImage", js.FuncOf(exportImage))
	js.Global().Set("tryLoadWithPassword", js.FuncOf(tryLoadWithPassword))
	js.Global().Set("clearCanvas", js.FuncOf(clearCanvas))
	js.Global().Set("fillCanvas", js.FuncOf(fillCanvas))
	js.Global().Set("loadImageData", js.FuncOf(loadImageDataJS))
	js.Global().Set("resizeCanvas", js.FuncOf(resizeCanvasJS))
	js.Global().Set("undoCanvas", js.FuncOf(undoJS))
	js.Global().Set("redoCanvas", js.FuncOf(redoJS))
	js.Global().Set("canUndoCanvas", js.FuncOf(canUndoJS))
	js.Global().Set("canRedoCanvas", js.FuncOf(canRedoJS))

	select {}
}

func parseIntSafe(s string, defaultVal int) int {
	val := 0
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			val = val*10 + int(s[i]-'0')
		} else {
			return defaultVal
		}
	}
	if val == 0 {
		return defaultVal
	}
	return val
}

// canvasCoords converts mouse client coordinates to canvas pixel coordinates,
// correctly accounting for object-fit: contain letterboxing and pillarboxing.
// This is the single source of truth used by every mouse handler.
func canvasCoords(clientX, clientY int, rect js.Value) (int, int) {
	rectLeft := rect.Get("left").Float()
	rectTop := rect.Get("top").Float()
	rectWidth := rect.Get("width").Float()
	rectHeight := rect.Get("height").Float()

	canvasAspect := float64(canvasWidth) / float64(canvasHeight)
	rectAspect := rectWidth / rectHeight

	var displayWidth, displayHeight, offX, offY float64
	if canvasAspect > rectAspect {
		// Canvas wider than container: letterbox top/bottom.
		displayWidth = rectWidth
		displayHeight = rectWidth / canvasAspect
		offX = 0
		offY = (rectHeight - displayHeight) / 2
	} else {
		// Canvas taller than container: pillarbox left/right.
		displayHeight = rectHeight
		displayWidth = rectHeight * canvasAspect
		offX = (rectWidth - displayWidth) / 2
		offY = 0
	}

	x := int((float64(clientX) - rectLeft - offX) * float64(canvasWidth) / displayWidth)
	y := int((float64(clientY) - rectTop - offY) * float64(canvasHeight) / displayHeight)
	return x, y
}

func mouseDown(this js.Value, args []js.Value) interface{} {
	e := args[0]
	if e.Get("button").Int() == 2 {
		// Right-click: do nothing here. The browser also fires contextmenu after
		// mousedown, which is where the line is drawn. If we updated lastX/lastY
		// here we would overwrite the anchor before contextMenu can use it.
		return nil
	}
	// Left click: normal freehand drawing.
	rect := canvas.Call("getBoundingClientRect")
	lastX, lastY = canvasCoords(e.Get("clientX").Int(), e.Get("clientY").Int(), rect)
	drawing = true
	vecStartStroke(lastX, lastY)
	drawPoint(lastX, lastY)
	return nil
}

func mouseMove(this js.Value, args []js.Value) interface{} {
	if !drawing {
		return nil
	}
	e := args[0]
	rect := canvas.Call("getBoundingClientRect")
	x, y := canvasCoords(e.Get("clientX").Int(), e.Get("clientY").Int(), rect)
	vecAddPoint(x, y)
	drawLine(lastX, lastY, x, y)
	lastX, lastY = x, y
	return nil
}

func mouseUp(this js.Value, args []js.Value) interface{} {
	vecEndStroke()
	drawing = false
	return nil
}

func mouseLeave(this js.Value, args []js.Value) interface{} {
	if drawing {
		vecEndStroke() // commit current segment; re-entry will start a new one
		e := args[0]
		rect := canvas.Call("getBoundingClientRect")
		x, y := canvasCoords(e.Get("clientX").Int(), e.Get("clientY").Int(), rect)
		if x < 0 {
			x = 0
		}
		if x >= canvasWidth {
			x = canvasWidth - 1
		}
		if y < 0 {
			y = 0
		}
		if y >= canvasHeight {
			y = canvasHeight - 1
		}
		lastX, lastY = x, y
	}
	return nil
}

func mouseEnter(this js.Value, args []js.Value) interface{} {
	e := args[0]
	if e.Get("buttons").Int() == 1 && drawing {
		rect := canvas.Call("getBoundingClientRect")
		x, y := canvasCoords(e.Get("clientX").Int(), e.Get("clientY").Int(), rect)
		vecStartStroke(lastX, lastY) // new segment from where we left off
		vecAddPoint(x, y)
		drawLine(lastX, lastY, x, y)
		lastX, lastY = x, y
	}
	return nil
}

// contextMenu fires on right-click. Draws a straight line from the last known
// position (lastX, lastY) to the click point, then updates lastX/lastY to that
// point so subsequent right-clicks chain lines. Suppresses the browser menu.
func contextMenu(this js.Value, args []js.Value) interface{} {
	e := args[0]
	e.Call("preventDefault")
	rect := canvas.Call("getBoundingClientRect")
	x, y := canvasCoords(e.Get("clientX").Int(), e.Get("clientY").Int(), rect)
	// End any in-progress freehand stroke cleanly before drawing the line.
	vecEndStroke()
	// Record and draw the straight line as a two-point stroke.
	vecStartStroke(lastX, lastY)
	vecAddPoint(x, y)
	vecEndStroke()
	drawLine(lastX, lastY, x, y)
	lastX, lastY = x, y
	return nil
}

func drawPoint(x, y int) {
	ctx.Set("fillStyle", colorToHex(penColor))
	ctx.Call("beginPath")
	ctx.Call("arc", x, y, penWidth/2, 0, 2*3.14159)
	ctx.Call("fill")
	updateImageData(x, y)
}

func drawLine(x0, y0, x1, y1 int) {
	ctx.Set("strokeStyle", colorToHex(penColor))
	ctx.Set("lineWidth", penWidth)
	ctx.Set("lineCap", "round")
	ctx.Call("beginPath")
	ctx.Call("moveTo", x0, y0)
	ctx.Call("lineTo", x1, y1)
	ctx.Call("stroke")
	updateImageDataLine(x0, y0, x1, y1)
}

func updateImageData(x, y int) {
	r := penWidth / 2
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy <= r*r {
				px, py := x+dx, y+dy
				if px >= 0 && px < canvasWidth && py >= 0 && py < canvasHeight {
					idx := py*imgData.Stride + px*4
					imgData.Pix[idx] = penColor.R
					imgData.Pix[idx+1] = penColor.G
					imgData.Pix[idx+2] = penColor.B
					imgData.Pix[idx+3] = penColor.A
				}
			}
		}
	}
}

func updateImageDataLine(x0, y0, x1, y1 int) {
	dx := abs(x1 - x0)
	dy := abs(y1 - y0)
	sx := -1
	if x0 < x1 {
		sx = 1
	}
	sy := -1
	if y0 < y1 {
		sy = 1
	}
	err := dx - dy

	for {
		updateImageData(x0, y0)
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x0 += sx
		}
		if e2 < dx {
			err += dx
			y0 += sy
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func setColor(this js.Value, args []js.Value) interface{} {
	r := args[0].Int()
	g := args[1].Int()
	b := args[2].Int()
	penColor = color.RGBA{uint8(r), uint8(g), uint8(b), 255}
	return nil
}

func setWidth(this js.Value, args []js.Value) interface{} {
	penWidth = args[0].Int()
	return nil
}

func clearCanvas(this js.Value, args []js.Value) interface{} {
	vecEndStroke()
	historyPush(vecCmdClear{})
	ctx.Set("fillStyle", "white")
	ctx.Call("fillRect", 0, 0, canvasWidth, canvasHeight)
	for i := range imgData.Pix {
		imgData.Pix[i] = 255
	}
	return nil
}

func fillCanvas(this js.Value, args []js.Value) interface{} {
	vecEndStroke()
	historyPush(vecCmdFill{r: penColor.R, g: penColor.G, b: penColor.B})
	ctx.Set("fillStyle", colorToHex(penColor))
	ctx.Call("fillRect", 0, 0, canvasWidth, canvasHeight)
	for y := 0; y < canvasHeight; y++ {
		for x := 0; x < canvasWidth; x++ {
			idx := y*imgData.Stride + x*4
			imgData.Pix[idx] = penColor.R
			imgData.Pix[idx+1] = penColor.G
			imgData.Pix[idx+2] = penColor.B
			imgData.Pix[idx+3] = penColor.A
		}
	}
	return nil
}

func colorToHex(c color.RGBA) string {
	hexDigits := "0123456789abcdef"
	return "#" +
		string(hexDigits[c.R>>4]) + string(hexDigits[c.R&0xf]) +
		string(hexDigits[c.G>>4]) + string(hexDigits[c.G&0xf]) +
		string(hexDigits[c.B>>4]) + string(hexDigits[c.B&0xf])
}

func exportImage(this js.Value, args []js.Value) interface{} {
	password := ""
	if len(args) > 0 && !args[0].IsNull() && !args[0].IsUndefined() {
		password = args[0].String()
	}

	vecEndStroke() // commit any in-progress stroke

	active := vecCmds[:historyPos]
	if len(active) == 0 {
		return ""
	}

	// Trim to the suffix after the last clear/fill: commands before a full-canvas
	// overwrite are invisible and would only inflate the URL.
	trimmed := trimHistory(active)

	// Serialise and FLATE-compress the command log.
	payload := encodeVecCmds(trimmed)

	var data []byte
	if password != "" {
		// Encrypt the compressed payload, prepend single encMagic byte.
		encrypted, err := encrypt(payload, password)
		if err != nil {
			return ""
		}
		data = append([]byte{encMagic}, encrypted...)
	} else {
		data = payload
	}

	return base64.RawURLEncoding.EncodeToString(data)
}

// encodeVecCmds serialises the command log into the binary wire format and
// FLATE-compresses it. Delta-coded coordinates cluster near zero, so FLATE
// achieves very high compression ratios on typical whiteboard content.
func encodeVecCmds(cmds []vecCmd) []byte {
	var raw bytes.Buffer
	raw.WriteByte(vecMagic)
	raw.WriteByte(vecVersion)
	binary.Write(&raw, binary.LittleEndian, uint16(len(cmds)))

	for _, cmd := range cmds {
		switch c := cmd.(type) {
		case *vecCmdStroke:
			raw.WriteByte(vecTagStroke)
			raw.WriteByte(c.r)
			raw.WriteByte(c.g)
			raw.WriteByte(c.b)
			raw.WriteByte(c.width)
			binary.Write(&raw, binary.LittleEndian, uint16(len(c.pts)))
			for i, p := range c.pts {
				if i == 0 {
					binary.Write(&raw, binary.LittleEndian, uint16(p[0]))
					binary.Write(&raw, binary.LittleEndian, uint16(p[1]))
				} else {
					binary.Write(&raw, binary.LittleEndian, p[0]) // int16 delta
					binary.Write(&raw, binary.LittleEndian, p[1])
				}
			}
		case vecCmdClear:
			raw.WriteByte(vecTagClear)
		case vecCmdFill:
			raw.WriteByte(vecTagFill)
			raw.WriteByte(c.r)
			raw.WriteByte(c.g)
			raw.WriteByte(c.b)
		}
	}

	var out bytes.Buffer
	flw, _ := flate.NewWriter(&out, 9)
	flw.Write(raw.Bytes())
	flw.Close()
	return out.Bytes()
}

func compressPlane(data []byte) []byte {
	// Apply RLE first for runs of identical bytes
	rleData := runLengthEncode(data)

	// Then compress with FLATE
	var buf bytes.Buffer
	// FLATE compression with level 9 (best compression)
	flw, _ := flate.NewWriter(&buf, 9)
	flw.Write(rleData)
	flw.Close()
	return buf.Bytes()
}

func decompressPlane(data []byte) ([]byte, error) {
	// Decompress FLATE first
	flr := flate.NewReader(bytes.NewReader(data))
	defer flr.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, flr); err != nil {
		return nil, err
	}

	// Then decode RLE
	return runLengthDecode(buf.Bytes()), nil
}

func runLengthEncode(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	var result bytes.Buffer
	i := 0

	for i < len(data) {
		currentByte := data[i]
		runLength := 1

		// Count consecutive identical bytes (max 255)
		for i+1 < len(data) && data[i+1] == currentByte && runLength < 255 {
			i++
			runLength++
		}

		if runLength >= 3 {
			// Use RLE for runs of 3 or more: [marker=255][byte][count]
			result.WriteByte(255) // RLE marker
			result.WriteByte(currentByte)
			result.WriteByte(byte(runLength))
		} else {
			// For short runs, write literally
			for j := 0; j < runLength; j++ {
				// If byte is 255 (marker), escape it
				if currentByte == 255 {
					result.WriteByte(255)
					result.WriteByte(255)
					result.WriteByte(1) // Run of 1
				} else {
					result.WriteByte(currentByte)
				}
			}
		}

		i++
	}

	return result.Bytes()
}

func runLengthDecode(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	var result bytes.Buffer
	i := 0

	for i < len(data) {
		if data[i] == 255 && i+2 < len(data) {
			// RLE sequence: [255][byte][count]
			byteVal := data[i+1]
			count := int(data[i+2])
			for j := 0; j < count; j++ {
				result.WriteByte(byteVal)
			}
			i += 3
		} else {
			// Literal byte
			result.WriteByte(data[i])
			i++
		}
	}

	return result.Bytes()
}

func encrypt(data []byte, password string) ([]byte, error) {
	// Use PBKDF2-like key derivation (SHA-256 is stable and well-tested)
	key := sha256.Sum256([]byte(password))

	// AES-256-GCM is a NIST-approved, stable encryption algorithm
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}

	// GCM provides authenticated encryption (prevents tampering)
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	// Generate cryptographically secure random nonce
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	// Encrypt and authenticate: nonce || ciphertext || auth_tag
	ciphertext := gcm.Seal(nonce, nonce, data, nil)
	return ciphertext, nil
}

func decrypt(data []byte, password string) ([]byte, error) {
	// Derive same key from password
	key := sha256.Sum256([]byte(password))

	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return nil, errors.New("invalid ciphertext")
	}

	// Extract nonce and ciphertext
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]

	// Decrypt and verify authentication tag
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}

	return plaintext, nil
}

func loadFromURL() {
	urlParams := js.Global().Get("URLSearchParams").New(js.Global().Get("location").Get("search"))
	if !urlParams.Call("has", "img").Bool() {
		return
	}
	data := urlParams.Call("get", "img").String()
	if data == "" {
		return
	}
	decoded, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(data)
		if err != nil {
			js.Global().Call("alert", "Failed to decode image data: Invalid base64 format")
			return
		}
	}
	// First byte == encMagic means AES-GCM encrypted - show password modal.
	// Also handle legacy "ENC:" prefix for old URLs.
	if (len(decoded) >= 1 && decoded[0] == encMagic) ||
		(len(decoded) >= 4 && string(decoded[:4]) == "ENC:") {
		js.Global().Call("eval", "if(typeof passwordModalInstance !== 'undefined') passwordModalInstance.show();")
		return
	}
	if !loadImageData(decoded) {
		js.Global().Call("alert", "Failed to load image: Invalid image data format")
	}
}

func tryLoadWithPassword(this js.Value, args []js.Value) interface{} {
	if len(args) == 0 {
		return false
	}
	password := args[0].String()

	urlParams := js.Global().Get("URLSearchParams").New(js.Global().Get("location").Get("search"))
	if !urlParams.Call("has", "img").Bool() {
		js.Global().Call("alert", "No image data found in URL")
		return false
	}
	data := urlParams.Call("get", "img").String()
	if data == "" {
		js.Global().Call("alert", "Image data is empty")
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(data)
		if err != nil {
			js.Global().Call("alert", "Failed to decode image data")
			return false
		}
	}

	// Strip the encryption marker byte (new format: encMagic; legacy: "ENC:").
	var ciphertext []byte
	if len(decoded) >= 1 && decoded[0] == encMagic {
		ciphertext = decoded[1:]
	} else if len(decoded) >= 4 && string(decoded[:4]) == "ENC:" {
		ciphertext = decoded[4:] // legacy back-compat
	} else {
		js.Global().Call("alert", "Image is not encrypted")
		return false
	}

	decrypted, err := decrypt(ciphertext, password)
	if err != nil {
		return false // wrong password - modal shows its own error
	}
	if loadImageData(decrypted) {
		js.Global().Call("eval", "if(typeof passwordModalInstance !== 'undefined') passwordModalInstance.hide();")
		return true
	}
	js.Global().Call("alert", "Failed to load decrypted image data")
	return false
}

// loadImageData dispatches on format.
// New vector format: the raw bytes are a FLATE stream; decompressed payload
//
//	starts with vecMagic 'V' + vecVersion 0x01.
//
// Legacy bitmap: raw bytes start with a uint16 offsetX header (not a FLATE stream).
// After decryption the plaintext is passed here directly, so we never see
// encMagic here - that byte is consumed by tryLoadWithPassword/loadFromURL.
func loadImageData(data []byte) bool {
	if len(data) == 0 {
		return false
	}

	// Try FLATE decompression to detect new vector format.
	flr := flate.NewReader(bytes.NewReader(data))
	var raw bytes.Buffer
	_, flateErr := io.Copy(&raw, flr)
	flr.Close()

	if flateErr == nil && raw.Len() >= 2 &&
		raw.Bytes()[0] == vecMagic && raw.Bytes()[1] == vecVersion {
		return replayVecCmds(raw.Bytes())
	}
	// Fall through to legacy bitmap decoder.
	return loadLegacyBitmapData(data)
}

// replayVecCmds decodes the uncompressed vector payload, replays all commands
// onto the canvas, and rebuilds vecCmds so the user can keep drawing.
func replayVecCmds(payload []byte) bool {
	if len(payload) < 4 {
		return false
	}
	cmdCount := int(binary.LittleEndian.Uint16(payload[2:4]))
	pos := 4

	ctx.Set("fillStyle", "white")
	ctx.Call("fillRect", 0, 0, canvasWidth, canvasHeight)
	for i := range imgData.Pix {
		imgData.Pix[i] = 255
	}
	vecCmds = nil
	historyPos = 0
	vecCurStroke = nil

	for i := 0; i < cmdCount; i++ {
		if pos >= len(payload) {
			return false
		}
		tag := payload[pos]
		pos++

		switch tag {
		case vecTagStroke:
			if pos+6 > len(payload) {
				return false
			}
			r := payload[pos]
			g := payload[pos+1]
			b := payload[pos+2]
			w := int(payload[pos+3])
			ptCount := int(binary.LittleEndian.Uint16(payload[pos+4 : pos+6]))
			pos += 6
			if ptCount == 0 {
				continue
			}
			if pos+4+(ptCount-1)*4 > len(payload) {
				return false
			}

			// Decode points.
			pts := make([][2]int, ptCount)
			x := int(binary.LittleEndian.Uint16(payload[pos : pos+2]))
			y := int(binary.LittleEndian.Uint16(payload[pos+2 : pos+4]))
			pts[0] = [2]int{x, y}
			pos += 4
			for j := 1; j < ptCount; j++ {
				x += int(int16(binary.LittleEndian.Uint16(payload[pos : pos+2])))
				y += int(int16(binary.LittleEndian.Uint16(payload[pos+2 : pos+4])))
				pts[j] = [2]int{x, y}
				pos += 4
			}

			// Replay onto canvas.
			hex := colorToHex(color.RGBA{r, g, b, 255})
			if ptCount == 1 {
				ctx.Set("fillStyle", hex)
				ctx.Call("beginPath")
				ctx.Call("arc", pts[0][0], pts[0][1], w/2, 0, 2*3.14159)
				ctx.Call("fill")
			} else {
				ctx.Set("strokeStyle", hex)
				ctx.Set("lineWidth", w)
				ctx.Set("lineCap", "round")
				ctx.Set("lineJoin", "round")
				ctx.Call("beginPath")
				ctx.Call("moveTo", pts[0][0], pts[0][1])
				for _, p := range pts[1:] {
					ctx.Call("lineTo", p[0], p[1])
				}
				ctx.Call("stroke")
			}

			// Rebuild vecCmds entry.
			vs := &vecCmdStroke{r: r, g: g, b: b, width: byte(w),
				absX: pts[ptCount-1][0], absY: pts[ptCount-1][1]}
			vs.pts = append(vs.pts, [2]int16{int16(pts[0][0]), int16(pts[0][1])})
			for j := 1; j < ptCount; j++ {
				dx := pts[j][0] - pts[j-1][0]
				dy := pts[j][1] - pts[j-1][1]
				vs.pts = append(vs.pts, [2]int16{int16(dx), int16(dy)})
			}
			historyPush(vs)

		case vecTagClear:
			ctx.Set("fillStyle", "white")
			ctx.Call("fillRect", 0, 0, canvasWidth, canvasHeight)
			for i := range imgData.Pix {
				imgData.Pix[i] = 255
			}
			historyPush(vecCmdClear{})

		case vecTagFill:
			if pos+3 > len(payload) {
				return false
			}
			r := payload[pos]
			g := payload[pos+1]
			b := payload[pos+2]
			pos += 3
			hex := colorToHex(color.RGBA{r, g, b, 255})
			ctx.Set("fillStyle", hex)
			ctx.Call("fillRect", 0, 0, canvasWidth, canvasHeight)
			for i := 0; i < len(imgData.Pix); i += 4 {
				imgData.Pix[i] = r
				imgData.Pix[i+1] = g
				imgData.Pix[i+2] = b
				imgData.Pix[i+3] = 255
			}
			historyPush(vecCmdFill{r: r, g: g, b: b})

		default:
			return false // unknown tag - corrupt data
		}
	}

	// Sync imgData from canvas (Canvas2D is authoritative after replay).
	jsID := ctx.Call("getImageData", 0, 0, canvasWidth, canvasHeight)
	js.CopyBytesToGo(imgData.Pix, jsID.Get("data"))
	return true
}

// loadLegacyBitmapData is the original decoder, kept verbatim for back-compat
// with URLs generated before the vector format was introduced.
func loadLegacyBitmapData(data []byte) bool {
	buf := bytes.NewReader(data)
	var offsetX, offsetY, width, height uint16
	if err := binary.Read(buf, binary.LittleEndian, &offsetX); err != nil {
		return false
	}
	if err := binary.Read(buf, binary.LittleEndian, &offsetY); err != nil {
		return false
	}
	if err := binary.Read(buf, binary.LittleEndian, &width); err != nil {
		return false
	}
	if err := binary.Read(buf, binary.LittleEndian, &height); err != nil {
		return false
	}
	compressedData := make([]byte, buf.Len())
	if _, err := buf.Read(compressedData); err != nil {
		return false
	}
	interleavedBits, err := decompressPlane(compressedData)
	if err != nil {
		return false
	}

	ctx.Set("fillStyle", "white")
	ctx.Call("fillRect", 0, 0, canvasWidth, canvasHeight)
	for i := range imgData.Pix {
		imgData.Pix[i] = 255
	}

	bitIdx := 0
	for y := 0; y < int(height); y++ {
		for x := 0; x < int(width); x++ {
			getBit := func() byte {
				bi := bitIdx / 8
				bp := uint(7 - (bitIdx % 8))
				bitIdx++
				if bi < len(interleavedBits) && (interleavedBits[bi]&(1<<bp)) != 0 {
					return 255
				}
				return 0
			}
			r, g, b2 := getBit(), getBit(), getBit()
			destX := int(offsetX) + x
			destY := int(offsetY) + y
			if destX < canvasWidth && destY < canvasHeight {
				idx := destY*imgData.Stride + destX*4
				imgData.Pix[idx] = r
				imgData.Pix[idx+1] = g
				imgData.Pix[idx+2] = b2
				imgData.Pix[idx+3] = 255
			}
		}
	}
	imgJSData := ctx.Call("createImageData", canvasWidth, canvasHeight)
	js.CopyBytesToJS(imgJSData.Get("data"), imgData.Pix)
	ctx.Call("putImageData", imgJSData, 0, 0)
	return true
}

func loadImageDataJS(this js.Value, args []js.Value) interface{} {
	if len(args) == 0 {
		return false
	}

	// Get Uint8Array from JavaScript
	jsArray := args[0]
	length := jsArray.Get("length").Int()
	data := make([]byte, length)
	js.CopyBytesToGo(data, jsArray)

	return loadImageData(data)
}

func resizeCanvasJS(this js.Value, args []js.Value) interface{} {
	if len(args) < 2 {
		return false
	}

	newWidth := args[0].Int()
	newHeight := args[1].Int()

	// Validate dimensions
	if newWidth < 64 || newWidth > 2048 || newHeight < 64 || newHeight > 2048 {
		return false
	}

	// Save current image data
	oldData := make([]byte, len(imgData.Pix))
	copy(oldData, imgData.Pix)
	oldWidth := canvasWidth
	oldHeight := canvasHeight

	// Update canvas dimensions
	canvasWidth = newWidth
	canvasHeight = newHeight

	// Create new image data
	imgData = image.NewRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))

	// Fill with white
	for i := range imgData.Pix {
		imgData.Pix[i] = 255
	}

	// Update canvas element
	canvas.Set("width", canvasWidth)
	canvas.Set("height", canvasHeight)

	// Clear canvas
	ctx.Set("fillStyle", "white")
	ctx.Call("fillRect", 0, 0, canvasWidth, canvasHeight)

	// Center the old content in the new canvas.
	offsetX := (canvasWidth - oldWidth) / 2
	offsetY := (canvasHeight - oldHeight) / 2
	if offsetX < 0 {
		offsetX = 0
	}
	if offsetY < 0 {
		offsetY = 0
	}

	// Copy pixel data with centering offset applied.
	for y := 0; y < oldHeight && y < canvasHeight; y++ {
		for x := 0; x < oldWidth && x < canvasWidth; x++ {
			srcIdx := y*oldWidth*4 + x*4
			destX := x + offsetX
			destY := y + offsetY
			if destX >= 0 && destX < canvasWidth && destY >= 0 && destY < canvasHeight {
				dstIdx := destY*imgData.Stride + destX*4
				if srcIdx+3 < len(oldData) {
					imgData.Pix[dstIdx] = oldData[srcIdx]
					imgData.Pix[dstIdx+1] = oldData[srcIdx+1]
					imgData.Pix[dstIdx+2] = oldData[srcIdx+2]
					imgData.Pix[dstIdx+3] = oldData[srcIdx+3]
				}
			}
		}
	}

	// Shift all recorded stroke coordinates by the same offset applied to pixels.
	// Without this the vector history still references old-canvas positions, so
	// export/replay would draw strokes at the wrong location after a resize.
	if offsetX != 0 || offsetY != 0 {
		shiftVecCmds(offsetX, offsetY, historyPos)
	}

	// Update canvas display.
	imgJSData := ctx.Call("createImageData", canvasWidth, canvasHeight)
	data8 := imgJSData.Get("data")
	js.CopyBytesToJS(data8, imgData.Pix)
	ctx.Call("putImageData", imgJSData, 0, 0)

	return true
}

// shiftVecCmds translates all stroke coordinates in the vector history by
// (dx, dy). Called after a resize that centers the old content, so the vector
// record stays in sync with the visual pixel positions on the new canvas.
// trimHistory returns the minimal suffix of cmds that produces the same visual
// result: everything before the last CMD_CLEAR or CMD_FILL is invisible.
func trimHistory(cmds []vecCmd) []vecCmd {
	last := 0
	for i, cmd := range cmds {
		switch cmd.(type) {
		case vecCmdClear, vecCmdFill:
			last = i
		}
	}
	// Keep from the last full-canvas overwrite onward (inclusive).
	switch cmds[last].(type) {
	case vecCmdClear, vecCmdFill:
		return cmds[last:]
	}
	return cmds
}

// applyHistoryAt replays vecCmds[0:pos] onto a blank canvas.
// Used by both undo and redo.
func applyHistoryAt(pos int) {
	ctx.Set("fillStyle", "white")
	ctx.Call("fillRect", 0, 0, canvasWidth, canvasHeight)
	for i := range imgData.Pix {
		imgData.Pix[i] = 255
	}
	for _, cmd := range vecCmds[:pos] {
		switch c := cmd.(type) {
		case *vecCmdStroke:
			hex := colorToHex(color.RGBA{c.r, c.g, c.b, 255})
			w := int(c.width)
			// Reconstruct absolute points from delta encoding.
			x := int(c.pts[0][0])
			y := int(c.pts[0][1])
			if len(c.pts) == 1 {
				ctx.Set("fillStyle", hex)
				ctx.Call("beginPath")
				ctx.Call("arc", x, y, w/2, 0, 2*3.14159)
				ctx.Call("fill")
			} else {
				ctx.Set("strokeStyle", hex)
				ctx.Set("lineWidth", w)
				ctx.Set("lineCap", "round")
				ctx.Set("lineJoin", "round")
				ctx.Call("beginPath")
				ctx.Call("moveTo", x, y)
				for _, p := range c.pts[1:] {
					x += int(p[0])
					y += int(p[1])
					ctx.Call("lineTo", x, y)
				}
				ctx.Call("stroke")
			}
		case vecCmdClear:
			ctx.Set("fillStyle", "white")
			ctx.Call("fillRect", 0, 0, canvasWidth, canvasHeight)
			for i := range imgData.Pix {
				imgData.Pix[i] = 255
			}
		case vecCmdFill:
			hex := colorToHex(color.RGBA{c.r, c.g, c.b, 255})
			ctx.Set("fillStyle", hex)
			ctx.Call("fillRect", 0, 0, canvasWidth, canvasHeight)
			for i := 0; i < len(imgData.Pix); i += 4 {
				imgData.Pix[i] = c.r
				imgData.Pix[i+1] = c.g
				imgData.Pix[i+2] = c.b
				imgData.Pix[i+3] = 255
			}
		}
	}
	// Sync imgData from canvas after replay.
	jsID := ctx.Call("getImageData", 0, 0, canvasWidth, canvasHeight)
	js.CopyBytesToGo(imgData.Pix, jsID.Get("data"))
}

// undoJS undoes the last committed command. Returns true if undo was possible.
func undoJS(this js.Value, args []js.Value) interface{} {
	vecEndStroke() // commit any in-progress stroke first
	if historyPos == 0 {
		return false
	}
	historyPos--
	applyHistoryAt(historyPos)
	return true
}

// redoJS re-applies the next command after the current position.
// Returns true if redo was possible.
func redoJS(this js.Value, args []js.Value) interface{} {
	if historyPos >= len(vecCmds) {
		return false
	}
	historyPos++
	applyHistoryAt(historyPos)
	return true
}

// canUndoJS returns true when there is at least one command to undo.
func canUndoJS(this js.Value, args []js.Value) interface{} {
	return historyPos > 0
}

// canRedoJS returns true when there are commands ahead of the current position.
func canRedoJS(this js.Value, args []js.Value) interface{} {
	return historyPos < len(vecCmds)
}

func shiftVecCmds(dx, dy, limit int) {
	for _, cmd := range vecCmds[:limit] {
		s, ok := cmd.(*vecCmdStroke)
		if !ok || len(s.pts) == 0 {
			continue
		}
		// Rebuild absolute coordinates, shift each one, re-encode as deltas.
		// pts[0] is stored as absolute (x, y); pts[1..] are int16 deltas.
		abs := make([][2]int, len(s.pts))
		abs[0][0] = int(s.pts[0][0]) + dx
		abs[0][1] = int(s.pts[0][1]) + dy
		for i := 1; i < len(s.pts); i++ {
			abs[i][0] = abs[i-1][0] + int(s.pts[i][0])
			abs[i][1] = abs[i-1][1] + int(s.pts[i][1])
		}
		// Re-encode: first point absolute, rest as int16 deltas.
		s.pts[0] = [2]int16{int16(abs[0][0]), int16(abs[0][1])}
		for i := 1; i < len(abs); i++ {
			ddx := abs[i][0] - abs[i-1][0]
			ddy := abs[i][1] - abs[i-1][1]
			if ddx > 32767 {
				ddx = 32767
			}
			if ddx < -32768 {
				ddx = -32768
			}
			if ddy > 32767 {
				ddy = 32767
			}
			if ddy < -32768 {
				ddy = -32768
			}
			s.pts[i] = [2]int16{int16(ddx), int16(ddy)}
		}
		// Keep absX/absY in sync with the last point's new position.
		s.absX += dx
		s.absY += dy
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
