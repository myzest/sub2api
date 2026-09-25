package basispoints

import (
	"fmt"
	"mime"
	"slices"
	"strings"
)

// Only protocol shape and byte counts, never URLs, file IDs or image content.
type imageDiagnostic struct {
	Path         string `json:"path"`
	Type         string `json:"type"`
	Source       string `json:"source"`
	Detail       string `json:"detail"`
	MIME         string `json:"mime,omitempty"`
	EncodedBytes int    `json:"encoded_bytes,omitempty"`
}

func summarizeImages(source object) ([]imageDiagnostic, int) {
	var result []imageDiagnostic
	count := 0
	add := func(part object, path string) {
		kind := str(part, "type")
		if kind != "input_image" && kind != "image_url" && kind != "image" {
			return
		}
		count++
		if len(result) >= 16 {
			return
		}
		d := imageDiagnostic{Path: path, Type: kind, Source: "missing", Detail: "absent"}
		imageURL := part["image_url"]
		detail, hasDetail := part["detail"]
		if nested, ok := imageURL.(object); ok {
			imageURL = nested["url"]
			if !hasDetail {
				detail, hasDetail = nested["detail"]
			}
			d.Source = "object"
		}
		if hasDetail {
			d.Detail = "invalid"
			if detail == nil {
				d.Detail = "null"
			} else if value, ok := detail.(string); ok {
				switch value {
				case "auto", "low", "high", "original":
					d.Detail = value
				}
			}
		}
		if value, ok := imageURL.(string); ok {
			prefix := ""
			if d.Source == "object" {
				prefix = "object."
			}
			d.Source = prefix + "url"
			d.EncodedBytes = len(value)
			if len(value) >= 5 && strings.EqualFold(value[:5], "data:") {
				d.Source = prefix + "data_url"
				header, _, _ := strings.Cut(value[5:], ",")
				header = strings.TrimSuffix(strings.ToLower(header), ";base64")
				media, _, err := mime.ParseMediaType(header)
				d.MIME = "other"
				if err == nil {
					switch media {
					case "image/png", "image/jpeg", "image/jpg", "image/webp", "image/gif", "image/heic", "image/heif", "image/avif", "image/bmp", "image/tiff", "image/svg+xml":
						d.MIME = media
					}
				}
			}
		} else if imageURL != nil && d.Source != "object" {
			d.Source = "invalid_image_url"
		}
		if str(part, "file_id") != "" {
			if d.Source == "missing" {
				d.Source = "file_id"
			} else {
				d.Source += "+file_id"
			}
		}
		result = append(result, d)
	}
	var walk func(any, string)
	walk = func(value any, path string) {
		switch v := value.(type) {
		case []any:
			for index, item := range v {
				walk(item, fmt.Sprintf("%s[%d]", path, index))
			}
		case object:
			if str(v, "type") == "additional_tools" {
				return
			} // Tool schema examples are not input pictures.
			add(v, path)
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			for _, key := range keys {
				walk(v[key], path+"."+imagePathKey(key))
			}
		}
	}
	walk(source["input"], "input")
	pictures, _ := source["images"].([]any)
	for index, raw := range pictures {
		if picture, ok := raw.(object); ok {
			part := clone(picture)
			part["type"] = "image"
			add(part, fmt.Sprintf("images[%d]", index))
		}
	}
	return result, count
}

func imagePathKey(key string) string {
	switch key {
	case "content", "output", "images", "image":
		return key
	}
	return "*" // Arbitrary object keys may themselves contain private data.
}
