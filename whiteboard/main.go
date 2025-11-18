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

var (
	canvas    js.Value
	ctx       js.Value
	drawing   bool
	lastX, lastY int
	penColor  color.RGBA = color.RGBA{0, 0, 0, 255}
	penWidth  int = 2
	imgData   *image.RGBA
	canvasWidth, canvasHeight int
)

func main() {
	// Parse canvas size from URL params
	canvasWidth, canvasHeight = 512, 512
	urlParams := js.Global().Get("URLSearchParams").New(js.Global().Get("location").Get("search"))
	if urlParams.Call("has", "w").Bool() {
		w := urlParams.Call("get", "w").String()
		if val := parseIntSafe(w, 512); val >= 64 && val <= 2048 {
			canvasWidth = val
		}
	}
	if urlParams.Call("has", "h").Bool() {
		h := urlParams.Call("get", "h").String()
		if val := parseIntSafe(h, 512); val >= 64 && val <= 2048 {
			canvasHeight = val
		}
	}
	
	imgData = image.NewRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))
	for i := range imgData.Pix {
		imgData.Pix[i] = 255
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

	js.Global().Set("setColor", js.FuncOf(setColor))
	js.Global().Set("setWidth", js.FuncOf(setWidth))
	js.Global().Set("exportImage", js.FuncOf(exportImage))
	js.Global().Set("tryLoadWithPassword", js.FuncOf(tryLoadWithPassword))
	js.Global().Set("clearCanvas", js.FuncOf(clearCanvas))
	js.Global().Set("fillCanvas", js.FuncOf(fillCanvas))
	js.Global().Set("loadImageData", js.FuncOf(loadImageDataJS))
	js.Global().Set("resizeCanvas", js.FuncOf(resizeCanvasJS))

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

func mouseDown(this js.Value, args []js.Value) interface{} {
	e := args[0]
	rect := canvas.Call("getBoundingClientRect")
	
	// Get client coordinates
	clientX := e.Get("clientX").Int()
	clientY := e.Get("clientY").Int()
	
	// Get canvas position
	rectLeft := rect.Get("left").Int()
	rectTop := rect.Get("top").Int()
	
	// Get canvas display size and actual size
	rectWidth := rect.Get("width").Float()
	rectHeight := rect.Get("height").Float()
	
	// Calculate scale factors
	scaleX := float64(canvasWidth) / rectWidth
	scaleY := float64(canvasHeight) / rectHeight
	
	// Calculate actual canvas coordinates
	lastX = int(float64(clientX-rectLeft) * scaleX)
	lastY = int(float64(clientY-rectTop) * scaleY)
	
	drawing = true
	drawPoint(lastX, lastY)
	return nil
}

func mouseMove(this js.Value, args []js.Value) interface{} {
	if !drawing {
		return nil
	}
	e := args[0]
	rect := canvas.Call("getBoundingClientRect")
	
	// Get client coordinates
	clientX := e.Get("clientX").Int()
	clientY := e.Get("clientY").Int()
	
	// Get canvas position
	rectLeft := rect.Get("left").Int()
	rectTop := rect.Get("top").Int()
	
	// Get canvas display size and actual size
	rectWidth := rect.Get("width").Float()
	rectHeight := rect.Get("height").Float()
	
	// Calculate scale factors
	scaleX := float64(canvasWidth) / rectWidth
	scaleY := float64(canvasHeight) / rectHeight
	
	// Calculate actual canvas coordinates
	x := int(float64(clientX-rectLeft) * scaleX)
	y := int(float64(clientY-rectTop) * scaleY)
	
	drawLine(lastX, lastY, x, y)
	lastX, lastY = x, y
	return nil
}

func mouseUp(this js.Value, args []js.Value) interface{} {
	drawing = false
	return nil
}

func mouseLeave(this js.Value, args []js.Value) interface{} {
	// Don't stop drawing when mouse leaves canvas
	// Just update the last position if we're drawing
	if drawing {
		e := args[0]
		rect := canvas.Call("getBoundingClientRect")
		
		clientX := e.Get("clientX").Int()
		clientY := e.Get("clientY").Int()
		rectLeft := rect.Get("left").Int()
		rectTop := rect.Get("top").Int()
		rectWidth := rect.Get("width").Float()
		rectHeight := rect.Get("height").Float()
		
		scaleX := float64(canvasWidth) / rectWidth
		scaleY := float64(canvasHeight) / rectHeight
		
		lastX = int(float64(clientX-rectLeft) * scaleX)
		lastY = int(float64(clientY-rectTop) * scaleY)
		
		// Clamp to canvas boundaries
		if lastX < 0 {
			lastX = 0
		}
		if lastX >= canvasWidth {
			lastX = canvasWidth - 1
		}
		if lastY < 0 {
			lastY = 0
		}
		if lastY >= canvasHeight {
			lastY = canvasHeight - 1
		}
	}
	return nil
}

func mouseEnter(this js.Value, args []js.Value) interface{} {
	// Continue drawing if mouse button is still pressed when re-entering
	e := args[0]
	buttons := e.Get("buttons").Int()
	
	// buttons == 1 means left mouse button is pressed
	if buttons == 1 && drawing {
		rect := canvas.Call("getBoundingClientRect")
		
		clientX := e.Get("clientX").Int()
		clientY := e.Get("clientY").Int()
		rectLeft := rect.Get("left").Int()
		rectTop := rect.Get("top").Int()
		rectWidth := rect.Get("width").Float()
		rectHeight := rect.Get("height").Float()
		
		scaleX := float64(canvasWidth) / rectWidth
		scaleY := float64(canvasHeight) / rectHeight
		
		x := int(float64(clientX-rectLeft) * scaleX)
		y := int(float64(clientY-rectTop) * scaleY)
		
		// Draw line from last position to current position
		drawLine(lastX, lastY, x, y)
		lastX, lastY = x, y
	}
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
	// Clear to white
	ctx.Set("fillStyle", "white")
	ctx.Call("fillRect", 0, 0, canvasWidth, canvasHeight)
	
	// Reset image data to white
	for i := range imgData.Pix {
		imgData.Pix[i] = 255
	}
	
	return nil
}

func fillCanvas(this js.Value, args []js.Value) interface{} {
	// Fill with current pen color
	ctx.Set("fillStyle", colorToHex(penColor))
	ctx.Call("fillRect", 0, 0, canvasWidth, canvasHeight)
	
	// Update image data
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
	
	// Find bounding box of non-white pixels
	minX, minY := canvasWidth, canvasHeight
	maxX, maxY := 0, 0
	hasContent := false
	
	for y := 0; y < canvasHeight; y++ {
		for x := 0; x < canvasWidth; x++ {
			idx := y*imgData.Stride + x*4
			if imgData.Pix[idx] != 255 || imgData.Pix[idx+1] != 255 || imgData.Pix[idx+2] != 255 {
				hasContent = true
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	
	if !hasContent {
		return ""
	}
	
	// Crop to bounding box with small padding
	minX = max(0, minX-2)
	minY = max(0, minY-2)
	maxX = min(canvasWidth-1, maxX+2)
	maxY = min(canvasHeight-1, maxY+2)
	
	width := maxX - minX + 1
	height := maxY - minY + 1
	
	// Create binary planes for R, G, B channels
	numPixels := width * height
	numBytes := (numPixels + 7) / 8
	
	rPlane := make([]byte, numBytes)
	gPlane := make([]byte, numBytes)
	bPlane := make([]byte, numBytes)
	
	bitIdx := 0
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			srcIdx := (minY+y)*imgData.Stride + (minX+x)*4
			r := imgData.Pix[srcIdx]
			g := imgData.Pix[srcIdx+1]
			b := imgData.Pix[srcIdx+2]
			
			// Binarize: threshold at 128
			byteIdx := bitIdx / 8
			bitPos := uint(7 - (bitIdx % 8))
			
			if r > 128 {
				rPlane[byteIdx] |= (1 << bitPos)
			}
			if g > 128 {
				gPlane[byteIdx] |= (1 << bitPos)
			}
			if b > 128 {
				bPlane[byteIdx] |= (1 << bitPos)
			}
			
			bitIdx++
		}
	}
	
	// Compress each plane separately
	var buf bytes.Buffer
	
	// Write header: offsetX, offsetY, width, height (4 x uint16)
	binary.Write(&buf, binary.LittleEndian, uint16(minX))
	binary.Write(&buf, binary.LittleEndian, uint16(minY))
	binary.Write(&buf, binary.LittleEndian, uint16(width))
	binary.Write(&buf, binary.LittleEndian, uint16(height))
	
	// Compress and write R plane
	compressedR := compressPlane(rPlane)
	binary.Write(&buf, binary.LittleEndian, uint32(len(compressedR)))
	buf.Write(compressedR)
	
	// Compress and write G plane
	compressedG := compressPlane(gPlane)
	binary.Write(&buf, binary.LittleEndian, uint32(len(compressedG)))
	buf.Write(compressedG)
	
	// Compress and write B plane
	compressedB := compressPlane(bPlane)
	binary.Write(&buf, binary.LittleEndian, uint32(len(compressedB)))
	buf.Write(compressedB)
	
	data := buf.Bytes()
	
	// Encrypt if password provided
	if password != "" {
		encrypted, err := encrypt(data, password)
		if err != nil {
			return ""
		}
		// Add marker prefix to identify encrypted data: "ENC:"
		marker := []byte("ENC:")
		data = append(marker, encrypted...)
	}
	
	encoded := base64.RawURLEncoding.EncodeToString(data)
	return encoded
}

func compressPlane(data []byte) []byte {
	var buf bytes.Buffer
	// FLATE compression with level 9 (best compression)
	flw, _ := flate.NewWriter(&buf, 9)
	flw.Write(data)
	flw.Close()
	return buf.Bytes()
}

func decompressPlane(data []byte) ([]byte, error) {
	flr := flate.NewReader(bytes.NewReader(data))
	defer flr.Close()
	
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, flr); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
	
	// Check if data is encrypted (starts with "ENC:" marker)
	if len(decoded) >= 4 && string(decoded[:4]) == "ENC:" {
		// Password protected - trigger password modal via JavaScript
		js.Global().Call("eval", "if(typeof passwordModalInstance !== 'undefined') passwordModalInstance.show();")
		return
	}
	
	// Not encrypted - load directly
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
	
	// Check and remove "ENC:" marker
	if len(decoded) < 4 || string(decoded[:4]) != "ENC:" {
		js.Global().Call("alert", "Image is not encrypted")
		return false
	}
	decoded = decoded[4:]
	
	// Try to decrypt
	decrypted, err := decrypt(decoded, password)
	if err != nil {
		// Wrong password - don't show alert, just return false to show error in modal
		return false
	}
	
	// Try to load the decrypted data
	if loadImageData(decrypted) {
		// Close modal via Bootstrap API
		js.Global().Call("eval", "if(typeof passwordModalInstance !== 'undefined') passwordModalInstance.hide();")
		return true
	}
	
	js.Global().Call("alert", "Failed to load decrypted image data")
	return false
}

func loadImageData(data []byte) bool {
	buf := bytes.NewReader(data)
	
	// Read header: offsetX, offsetY, width, height
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
	
	// Read R plane
	var rLen uint32
	if err := binary.Read(buf, binary.LittleEndian, &rLen); err != nil {
		return false
	}
	rCompressed := make([]byte, rLen)
	if _, err := buf.Read(rCompressed); err != nil {
		return false
	}
	rPlane, err := decompressPlane(rCompressed)
	if err != nil {
		return false
	}
	
	// Read G plane
	var gLen uint32
	if err := binary.Read(buf, binary.LittleEndian, &gLen); err != nil {
		return false
	}
	gCompressed := make([]byte, gLen)
	if _, err := buf.Read(gCompressed); err != nil {
		return false
	}
	gPlane, err := decompressPlane(gCompressed)
	if err != nil {
		return false
	}
	
	// Read B plane
	var bLen uint32
	if err := binary.Read(buf, binary.LittleEndian, &bLen); err != nil {
		return false
	}
	bCompressed := make([]byte, bLen)
	if _, err := buf.Read(bCompressed); err != nil {
		return false
	}
	bPlane, err := decompressPlane(bCompressed)
	if err != nil {
		return false
	}
	
	// Clear canvas first
	ctx.Set("fillStyle", "white")
	ctx.Call("fillRect", 0, 0, canvasWidth, canvasHeight)
	for i := range imgData.Pix {
		imgData.Pix[i] = 255
	}
	
	// Reconstruct image from binary planes at original position
	bitIdx := 0
	for y := 0; y < int(height); y++ {
		for x := 0; x < int(width); x++ {
			byteIdx := bitIdx / 8
			bitPos := uint(7 - (bitIdx % 8))
			
			// Extract bits and convert to 0 or 255
			r := byte(0)
			if (rPlane[byteIdx] & (1 << bitPos)) != 0 {
				r = 255
			}
			
			g := byte(0)
			if (gPlane[byteIdx] & (1 << bitPos)) != 0 {
				g = 255
			}
			
			b := byte(0)
			if (bPlane[byteIdx] & (1 << bitPos)) != 0 {
				b = 255
			}
			
			// Place at original position using stored offset
			destX := int(offsetX) + x
			destY := int(offsetY) + y
			if destX < canvasWidth && destY < canvasHeight {
				idx := destY*imgData.Stride + destX*4
				imgData.Pix[idx] = r
				imgData.Pix[idx+1] = g
				imgData.Pix[idx+2] = b
				imgData.Pix[idx+3] = 255
			}
			
			bitIdx++
		}
	}
	
	imgJSData := ctx.Call("createImageData", canvasWidth, canvasHeight)
	data8 := imgJSData.Get("data")
	js.CopyBytesToJS(data8, imgData.Pix)
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
	
	// Copy old image centered
	offsetX := (canvasWidth - oldWidth) / 2
	offsetY := (canvasHeight - oldHeight) / 2
	if offsetX < 0 {
		offsetX = 0
	}
	if offsetY < 0 {
		offsetY = 0
	}
	
	// Copy pixel data
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
	
	// Update canvas display
	imgJSData := ctx.Call("createImageData", canvasWidth, canvasHeight)
	data8 := imgJSData.Get("data")
	js.CopyBytesToJS(data8, imgData.Pix)
	ctx.Call("putImageData", imgJSData, 0, 0)
	
	return true
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
