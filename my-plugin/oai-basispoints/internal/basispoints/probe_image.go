package basispoints

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"net/http"
	"regexp"
	"strings"
)

// The expected answer exists only in these pixels and local probe status. It
// is never inserted into the text prompt or image metadata/filename.
func probeImage(format string) (dataURL, expected string, err error) {
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", "", errors.New("无法生成图片探测样本")
	}
	digits := make([]byte, len(random))
	for i, value := range random {
		digits[i] = '0' + value%10
	}
	glyphs := [10][7]uint8{
		{14, 17, 19, 21, 25, 17, 14},
		{4, 12, 4, 4, 4, 4, 14},
		{14, 17, 1, 2, 4, 8, 31},
		{30, 1, 1, 14, 1, 1, 30},
		{2, 6, 10, 18, 31, 2, 2},
		{31, 16, 16, 30, 1, 1, 30},
		{14, 16, 16, 30, 17, 17, 14},
		{31, 1, 2, 4, 8, 8, 8},
		{14, 17, 17, 14, 17, 17, 14},
		{14, 17, 17, 15, 1, 1, 14},
	}
	const scale, margin = 10, 30
	picture := image.NewRGBA(image.Rect(0, 0, 6*6*scale+2*margin-scale, 7*scale+2*margin))
	draw.Draw(picture, picture.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	ink := image.NewUniform(color.Black)
	for i, digit := range digits {
		for y, row := range glyphs[digit-'0'] {
			for x := 0; x < 5; x++ {
				if row&(1<<uint(4-x)) == 0 {
					continue
				}
				left, top := margin+(i*6+x)*scale, margin+y*scale
				draw.Draw(picture, image.Rect(left, top, left+scale, top+scale), ink, image.Point{}, draw.Src)
			}
		}
	}
	var encoded bytes.Buffer
	switch format {
	case "jpeg":
		err = jpeg.Encode(&encoded, picture, &jpeg.Options{Quality: 95})
	case "png":
		err = png.Encode(&encoded, picture)
	default:
		return "", "", errors.New("图片探测格式只允许 jpeg/png")
	}
	if err != nil {
		return "", "", errors.New("无法编码图片探测样本")
	}
	return "data:image/" + format + ";base64," + base64.StdEncoding.EncodeToString(encoded.Bytes()), string(digits), nil
}

func probeSource(p *Probe) (object, error) {
	source := object{"model": p.Model, "stream": false, "reasoning": object{"effort": p.Effort}, "input": "Reply with exactly BPS_OK. This is a text connectivity check. Do not call any tools.", "tools": []any{}, "tool_choice": "none"}
	if p.Kind != "image" {
		return source, nil
	}
	if p.ImageFormat == "" {
		p.ImageFormat = "jpeg"
	}
	dataURL, expected, err := probeImage(p.ImageFormat)
	if err != nil {
		return nil, err
	}
	p.ImagePreview, p.ImageExpected = dataURL, expected
	// Desktop OAuth requests use SSE. Preserve the selected detail exactly so
	// high/original can be compared without changing production routing.
	source["stream"] = true
	source["input"] = []any{object{"type": "message", "role": "user", "content": []any{
		object{"type": "input_text", "text": "Read the six digits shown in the attached image from left to right. Reply only with those six digits, preserving any leading zeros. If you cannot see the image, reply CANNOT_READ_IMAGE. Do not call tools."},
		object{"type": "input_image", "image_url": dataURL, "detail": p.ImageDetail},
	}}}
	return source, nil
}

var probeBearer = regexp.MustCompile(`(?i)bearer\s+[^\s"',;]+`)
var probeJWT = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
var probeDataURL = regexp.MustCompile(`(?i)data:image/[^\s"']+`)
var probeEmail = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)

func redactProbeText(text string, headers http.Header, imageURL string) string {
	for _, key := range []string{"Authorization", "Chatgpt-Account-Id", "X-Openai-Account-Id", "X-Openai-Account-User-Id"} {
		secret := headers.Get(key)
		if key == "Authorization" {
			if parts := strings.Fields(secret); len(parts) == 2 {
				secret = parts[1]
			}
		}
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	if imageURL != "" {
		text = strings.ReplaceAll(text, imageURL, "[IMAGE]")
		if _, payload, ok := strings.Cut(imageURL, ","); ok && len(payload) > 32 {
			text = strings.ReplaceAll(text, payload, "[IMAGE]")
		}
	}
	text = probeBearer.ReplaceAllString(text, "Bearer [REDACTED]")
	text = probeJWT.ReplaceAllString(text, "[REDACTED]")
	text = probeEmail.ReplaceAllString(text, "[REDACTED]")
	text = probeDataURL.ReplaceAllString(text, "[IMAGE]")
	return limitCharacters(strings.TrimSpace(text), 1200)
}

// Follow the reference diagnostic boundary: retain validation paths/reasons,
// not rejected input, context, a whole HTML page or the raw response body.
func probeDiagnostic(value object, headers http.Header, imageURL string) string {
	return diagnosticFields(value, func(_ string, text string) string { return redactProbeText(text, headers, imageURL) })
}

func diagnosticFields(value object, redact func(string, string) string) string {
	safe := diagnosticObject(value, redact)
	if len(safe) == 0 {
		return ""
	}
	return limitCharacters(string(encoded(safe)), 1600)
}

func diagnosticObject(value object, redact func(string, string) string) object {
	if response, ok := value["response"].(object); ok {
		value = response
	}
	if nested, ok := value["error"].(object); ok {
		value = nested
	}
	safe := object{}
	for _, key := range []string{"message", "code", "param", "detail"} {
		if text := str(value, key); text != "" {
			safe[key] = redact(key, text)
		}
	}
	if text := str(value, "error"); text != "" {
		safe["message"] = redact("message", text)
	}
	if details, ok := value["detail"].([]any); ok {
		clean := []any{}
		for _, raw := range details[:min(len(details), 8)] {
			item, ok := raw.(object)
			if !ok {
				continue
			}
			entry := object{}
			for _, key := range []string{"msg", "type"} {
				if text := str(item, key); text != "" {
					entry[key] = redact(key, text)
				}
			}
			if loc, ok := item["loc"].([]any); ok {
				path := []any{}
				for _, part := range loc[:min(len(loc), 16)] {
					switch part := part.(type) {
					case string:
						path = append(path, redact("loc", part))
					case json.Number:
						path = append(path, part)
					}
				}
				entry["loc"] = path
			}
			clean = append(clean, entry)
		}
		safe["detail"] = clean
	}
	if len(safe) == 0 {
		return nil
	}
	if kind := str(value, "type"); kind != "" {
		safe["type"] = redact("type", kind)
	}
	return safe
}
