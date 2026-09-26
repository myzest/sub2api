package basispoints

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Adapt images.Pictures and Bridge._send_with_pictures from Excel 66c41df.
// Keep the already proven user-message attachment default. Tool-output
// pictures start inline, then learn per item kind from explicit HTTP 400/422.
type pictureFallback struct {
	server   *Server
	plan     *requestPlan
	original object
	headers  http.Header
	proxy    string
	refused  map[string]bool
	fresh    map[string]bool
	reused   map[string]bool
	inline   map[string]bool
	fileIDs  map[string]bool
	kinds    map[string]bool
	omit     bool
	down     bool
	retries  int
	omitted  int
	report   *attachmentReport
}

func (s *Server) newPictureFallback(plan *requestPlan, headers http.Header, proxy string) *pictureFallback {
	s.mu.RLock()
	refused := map[string]bool{"message": true}
	for kind := range s.refusedPictureKinds {
		refused[kind] = true
	}
	s.mu.RUnlock()
	return &pictureFallback{server: s, plan: plan, original: clone(plan.body), headers: headers, proxy: proxy, refused: refused, fresh: map[string]bool{}, kinds: map[string]bool{}, report: &attachmentReport{}}
}

func (p *pictureFallback) rewrite(ctx context.Context) error {
	p.reused, p.inline, p.fileIDs = map[string]bool{}, map[string]bool{}, map[string]bool{}
	p.down, p.omitted = false, 0
	input, _ := p.original["input"].([]any)
	rewritten := make([]any, len(input))
	for index, raw := range input {
		item, _ := raw.(object)
		kind := str(item, "type")
		if kind == "" {
			kind = "message"
		}
		value, err := p.walk(ctx, raw, kind, fmt.Sprintf("input[%d]", index))
		if err != nil {
			return err
		}
		rewritten[index] = value
	}
	p.plan.body = clone(p.original)
	p.plan.body["input"] = rewritten
	return nil
}

func (p *pictureFallback) walk(ctx context.Context, value any, kind, path string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch v := value.(type) {
	case []any:
		next := make([]any, len(v))
		for i, child := range v {
			var err error
			next[i], err = p.walk(ctx, child, kind, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
		}
		return next, nil
	case object:
		// Opaque encrypted parts are not picture containers. Their extension
		// fields belong to the upstream protocol and must not be rewritten.
		if strings.EqualFold(strings.TrimSpace(str(v, "type")), "encrypted_content") {
			return v, nil
		}
		url := str(v, "image_url")
		if str(v, "type") == "input_image" && len(url) >= 5 && strings.EqualFold(url[:5], "data:") {
			return p.picture(ctx, v, kind, path)
		}
		next := clone(v)
		// Stable traversal keeps attachment order and diagnostics deterministic.
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			child, err := p.walk(ctx, v[key], kind, path+"."+imagePathKey(key))
			if err != nil {
				return nil, err
			}
			next[key] = child
		}
		return next, nil
	}
	return value, nil
}

func (p *pictureFallback) omittedPart(reason string) object {
	p.omitted++
	return object{"type": "input_text", "text": "[image content omitted: " + reason + "]"}
}

func (p *pictureFallback) picture(ctx context.Context, part object, kind, path string) (any, error) {
	p.kinds[kind] = true
	if p.omit {
		return p.omittedPart("the Excel backend did not accept it"), nil
	}
	if !p.refused[kind] {
		p.inline[kind] = true
		return part, nil
	}
	// Reuse the established upload/cache/identity implementation for every
	// image position, including function/custom tool results.
	single := *p.plan
	single.body = object{"input": []any{object{"type": "message", "role": "user", "content": []any{part}}}}
	uploadCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	report, err := p.server.uploadInputImagesAvailable(uploadCtx, &single, p.headers, p.proxy, p.down)
	cancel()
	p.mergeReport(report, path)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var failed *attachmentError
		if errors.As(err, &failed) {
			if failed.code == "bps_attachment_transport" || report.DiagnosticState == "read_error" {
				p.down = true
			}
			return p.omittedPart("the picture could not be decoded or uploaded"), nil
		}
		return nil, err // Local URL/proxy/configuration errors are not an image refusal.
	}
	image := single.body["input"].([]any)[0].(object)["content"].([]any)[0].(object)
	id := str(image, "file_id")
	p.fileIDs[id] = true
	if report.Uploaded > 0 {
		p.fresh[id] = true
	} else if !p.fresh[id] {
		p.reused[id] = true
	}
	return image, nil
}

func (p *pictureFallback) mergeReport(next *attachmentReport, path string) {
	p.report.Uploaded += next.Uploaded
	p.report.Reused += next.Reused
	p.report.Attempts += next.Attempts
	if next.Attempts > 0 {
		p.report.HTTPStatus, p.report.ContentType, p.report.RequestID = next.HTTPStatus, next.ContentType, next.RequestID
		p.report.Diagnostic, p.report.DiagnosticState = next.Diagnostic, next.DiagnosticState
	}
	for _, item := range next.Images {
		item.Path = path
		if len(p.report.Images) < 16 {
			p.report.Images = append(p.report.Images, item)
		}
	}
}

func (p *pictureFallback) retry(status int) string {
	if (status != 400 && status != 422) || (len(p.inline) == 0 && len(p.fileIDs) == 0) || p.retries >= len(p.kinds)+2 {
		return ""
	}
	p.retries++
	if len(p.reused) > 0 {
		p.server.attachments.forgetIDs(p.reused)
		return "refresh_cached_attachments"
	}
	if len(p.inline) > 0 {
		kinds := make([]string, 0, len(p.inline))
		for kind := range p.inline {
			kinds = append(kinds, kind)
		}
		slices.Sort(kinds)
		kind := kinds[0]
		if p.inline["message"] {
			kind = "message"
		}
		p.refused[kind] = true
		p.server.mu.Lock()
		if p.server.refusedPictureKinds == nil {
			p.server.refusedPictureKinds = map[string]bool{}
		}
		// Only retain bounded protocol labels, not arbitrary client strings.
		if inputTypePattern.MatchString(kind) && len(p.server.refusedPictureKinds) < 64 {
			p.server.refusedPictureKinds[kind] = true
		}
		p.server.mu.Unlock()
		return "upload_rejected_inline_kind"
	}
	p.omit = true
	return "omit_rejected_images"
}

func (c *attachmentCache) forgetIDs(ids map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if ids[entry.Value.(attachmentEntry).id] {
			c.order.Remove(entry)
			delete(c.entries, key)
		}
	}
}
