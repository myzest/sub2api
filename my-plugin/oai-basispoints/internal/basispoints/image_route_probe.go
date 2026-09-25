package basispoints

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	pluginv1 "local.sub2api/oai-basispoints/internal/pluginapi"
)

// Forward recognizes this private callback and returns this request's completed
// diagnostic directly. Never read lastRequest: a real client can replace it.
type imageRouteProbeContextKey struct{}

func (s *Server) probeImageRoute(ctx context.Context, p *Probe, cfg Config, h http.Header, proxy string) error {
	started := time.Now()
	p.Round, p.Stage = 1, "request"
	p.HTTPStatus, p.RequestBytes = 0, 0
	p.RequestID, p.ContentType, p.UpstreamError, p.RouteDiagnosticID = "", "", "", ""
	p.ResponseID, p.ReturnedModel, p.Usage, p.Attachment = "", "", nil, nil
	defer func() {
		p.Rounds = append(p.Rounds, probeRoundResult{Round: p.Round, HTTPStatus: p.HTTPStatus, RequestID: p.RequestID, ResponseID: p.ResponseID, ReturnedModel: p.ReturnedModel, LatencyMS: time.Since(started).Milliseconds(), Usage: p.Usage})
		s.publishProbe(p)
	}()
	if !cfg.routes(p.AccountID) || !s.config().routes(p.AccountID) {
		return errors.New("图片路由探测要求已开启 BPS 路由，且所选账号位于路由白名单；本操作不会自动修改路由")
	}

	// Reuse the image-only sample, then add the same private tool carrier a
	// desktop request can deliver. The virtual declaration has no executor.
	sample := *p
	sample.Kind = "image"
	source, err := probeSource(&sample)
	if err != nil {
		return err
	}
	p.ImagePreview, p.ImageExpected = sample.ImagePreview, sample.ImageExpected
	p.ImageFormat = sample.ImageFormat
	p.ToolName, p.ToolType, p.ToolSource = "diagnostics.image_probe", "custom", "input.additional_tools"
	source["tool_choice"] = "auto"
	source["parallel_tool_calls"] = false
	source["prompt_cache_key"] = "bps-image-route-probe-" + p.ID
	delete(source, "tools")
	source["input"] = append(source["input"].([]any), object{
		"type": "additional_tools", "role": "developer", "tools": []any{
			object{"type": "namespace", "name": "diagnostics", "tools": []any{
				object{"type": "custom", "name": "image_probe", "description": "A virtual diagnostic tool with no executor. This image-only probe requires no tool calls. Do not invoke it.", "format": object{"type": "text"}},
			}},
		},
	})
	body := encoded(source)
	s.publishProbe(p)

	var captured *requestDiagnostic
	ctx = context.WithValue(ctx, imageRouteProbeContextKey{}, func(d *requestDiagnostic) { captured = d })
	headers := h.Clone()
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	stream := &imageRouteForwardStream{
		ctx: ctx, input: body,
		request: &pluginv1.ForwardRequestStart{
			RequestId: p.ID, Method: http.MethodPost, Url: "https://chatgpt.com/backend-api/codex/responses", Host: "chatgpt.com",
			Headers: headersToProto(headers), ProxyUrl: proxy, AccountId: p.AccountID,
			Platform: "openai", AccountType: "oauth", HasBody: true, ContentLength: int64(len(body)),
		},
	}
	forwardErr := s.Forward(stream)
	if captured != nil {
		p.RouteDiagnosticID, p.Stage = captured.ID, captured.Stage
		p.HTTPStatus, p.RequestID, p.ContentType = captured.HTTPStatus, captured.RequestID, captured.ContentType
		p.UpstreamError = captured.UpstreamError
		p.ImageTransport, p.RequestBytes = captured.ImageTransport, captured.UpstreamRequestBytes
		if captured.Attachment != nil {
			attachment := *captured.Attachment
			p.Attachment = &attachment
		}
	}
	if forwardErr != nil {
		if ctx.Err() != nil {
			return errors.New("图片路由探测已取消或超时")
		}
		return fmt.Errorf("图片路由转发失败：%s", redactProbeText(forwardErr.Error(), h, p.ImagePreview))
	}
	if stream.failure != nil {
		return fmt.Errorf("图片路由转发失败（%s）：%s", redactProbeText(stream.failure.Code, h, p.ImagePreview), redactProbeText(stream.failure.Message, h, p.ImagePreview))
	}
	response, err := stream.response()
	if err != nil {
		return fmt.Errorf("图片路由探测失败：%s", redactProbeText(err.Error(), h, p.ImagePreview))
	}
	if captured == nil {
		return errors.New("图片路由探测未取得本请求的路由诊断，无法确认已走 BPS")
	}
	if captured.OmittedImages > 0 {
		return errors.New("图片已按参考策略省略，本次只完成文本降级，不能判为识图探测通过；请查看图片诊断")
	}
	p.ResponseID, p.ReturnedModel = redactProbeText(str(response, "id"), h, p.ImagePreview), redactProbeText(str(response, "model"), h, p.ImagePreview)
	p.Usage = response["usage"]
	if str(response, "status") != "completed" {
		return errors.New("图片路由返回未完成的响应；请查看对应请求诊断")
	}
	p.ToolCalls = len(responseToolCalls(response))
	if p.ToolCalls > 0 {
		return errors.New("图片路由返回了工具调用；本探测不会执行客户端工具，尚未完成识图验收")
	}
	p.Stage = "image_check"
	text := responseText(response)
	p.ImageReply = redactProbeText(text, h, p.ImagePreview)
	if strings.TrimSpace(text) != p.ImageExpected {
		return errors.New("图片路由已响应，但回复与测试图中的六位数字不一致；请查看实际回复与对应请求诊断")
	}
	p.Stage = "completed"
	return nil
}

// This in-memory gRPC frame adapter enters Forward without a second listener,
// HTTP self-request or tool executor. Only the generated probe body is sent.
type imageRouteForwardStream struct {
	ctx          context.Context
	request      *pluginv1.ForwardRequestStart
	input        []byte
	requestSent  bool
	inputOffset  int
	inputEnded   bool
	responseHead *pluginv1.ForwardResponseStart
	output       bytes.Buffer
	ended        bool
	failure      *pluginv1.ForwardResponseError
}

var _ pluginv1.TransportPlugin_ForwardServer = (*imageRouteForwardStream)(nil)

func (s *imageRouteForwardStream) Context() context.Context { return s.ctx }
func (s *imageRouteForwardStream) Recv() (*pluginv1.ForwardRequest, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if !s.requestSent {
		s.requestSent = true
		return &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: s.request}}, nil
	}
	if s.inputOffset < len(s.input) {
		end := min(s.inputOffset+(32<<10), len(s.input))
		chunk := s.input[s.inputOffset:end]
		s.inputOffset = end
		return &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: chunk}}, nil
	}
	if !s.inputEnded {
		s.inputEnded = true
		return &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}}, nil
	}
	return nil, io.EOF
}

func (s *imageRouteForwardStream) Send(response *pluginv1.ForwardResponse) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if response == nil || s.ended || s.failure != nil {
		return errors.New("图片路由转发帧为空或出现在终态之后")
	}
	switch frame := response.Frame.(type) {
	case *pluginv1.ForwardResponse_Start:
		if frame.Start == nil || s.responseHead != nil {
			return errors.New("图片路由缺少响应头或重复开始响应")
		}
		s.responseHead = frame.Start
	case *pluginv1.ForwardResponse_BodyChunk:
		if s.responseHead == nil {
			return errors.New("图片路由正文早于响应头")
		}
		if len(frame.BodyChunk) > maxResponse-s.output.Len() {
			return errors.New("图片路由探测响应超过 32 MiB")
		}
		_, _ = s.output.Write(frame.BodyChunk)
	case *pluginv1.ForwardResponse_End:
		if frame.End == nil || s.responseHead == nil {
			return errors.New("图片路由缺少完整响应结束帧")
		}
		s.ended = true
	case *pluginv1.ForwardResponse_Error:
		if frame.Error == nil {
			return errors.New("图片路由错误帧为空")
		}
		s.failure = frame.Error
	default:
		return errors.New("图片路由转发帧类型无效")
	}
	return nil
}

func (s *imageRouteForwardStream) response() (object, error) {
	if s.responseHead == nil || !s.ended {
		return nil, errors.New("插件转发未完整返回响应")
	}
	if code := int(s.responseHead.StatusCode); code < 200 || code >= 300 {
		if body, err := decodeObject(s.output.Bytes()); err == nil {
			if failure, ok := body["error"].(object); ok && str(failure, "message") != "" {
				return nil, errors.New(str(failure, "message"))
			}
		}
		return nil, fmt.Errorf("插件转发返回 HTTP %d", code)
	}
	contentType, _, _ := mime.ParseMediaType(headersFromProto(s.responseHead.Headers).Get("Content-Type"))
	if contentType == "application/json" {
		return decodeObject(s.output.Bytes())
	}
	if contentType != "text/event-stream" {
		return nil, errors.New("插件转发未返回 JSON/SSE")
	}
	var result object
	err := readSSE(bytes.NewReader(s.output.Bytes()), func(event object) (bool, error) {
		switch str(event, "type") {
		case "response.completed", "response.failed", "response.incomplete":
			value, ok := event["response"].(object)
			if !ok || "response."+str(value, "status") != str(event, "type") {
				return false, errors.New("图片路由终态缺少有效 response")
			}
			result = value
			return true, nil
		case "error", "response.error":
			return false, errors.New("图片路由返回流内错误")
		default:
			return false, nil
		}
	})
	return result, err
}

func (*imageRouteForwardStream) SetHeader(metadata.MD) error  { return nil }
func (*imageRouteForwardStream) SendHeader(metadata.MD) error { return nil }
func (*imageRouteForwardStream) SetTrailer(metadata.MD)       {}
func (s *imageRouteForwardStream) SendMsg(message any) error {
	value, ok := message.(*pluginv1.ForwardResponse)
	if !ok {
		return errors.New("图片路由响应帧类型无效")
	}
	return s.Send(value)
}
func (s *imageRouteForwardStream) RecvMsg(message any) error {
	value, ok := message.(*pluginv1.ForwardRequest)
	if !ok || value == nil {
		return errors.New("图片路由请求帧类型无效")
	}
	frame, err := s.Recv()
	if err != nil {
		return err
	}
	proto.Reset(value)
	proto.Merge(value, frame)
	return nil
}
