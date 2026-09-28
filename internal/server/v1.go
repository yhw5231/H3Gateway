package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yhw5231/H3Gateway/internal/model"
	"github.com/yhw5231/H3Gateway/internal/pipeline"
	"github.com/yhw5231/H3Gateway/internal/store"
	"github.com/yhw5231/H3Gateway/internal/upstream"
)

// durationAliases maps any requested length onto the three profiles the trial
// channel exposes.
var durationAliases = map[int]int{
	4: 6, 5: 6, 6: 6, 7: 6,
	8: 10, 9: 10, 10: 10, 11: 10, 12: 10,
	13: 15, 14: 15, 15: 15, 16: 15, 20: 15,
}

var dataURLRe = regexp.MustCompile(`^data:(?P<mime>[\w/+.-]+);base64,(?P<b64>.+)$`)

// ---------------------------------------------------------------------------
// POST /v1/videos
// ---------------------------------------------------------------------------

type createVideoBody struct {
	Model    string          `json:"model"`
	Prompt   string          `json:"prompt"`
	Size     string          `json:"size"`
	Ratio    string          `json:"ratio"`
	Seconds  json.RawMessage `json:"seconds"`
	Duration json.RawMessage `json:"duration"`
	ImageURL json.RawMessage `json:"image_url"`
	Image    json.RawMessage `json:"image"`
}

func (s *Server) handleCreateVideo(w http.ResponseWriter, r *http.Request) {
	req, err := s.parseCreateRequest(r)
	if err != nil {
		var bad *requestError
		if errors.As(err, &bad) {
			writeOpenAIError(w, bad.status, bad.message, bad.code)
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "bad_request")
		return
	}

	task, err := s.pipe.Submit(r.Context(), *req)
	if err != nil {
		s.writeSubmitError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, taskToVideoObject(task, s.baseURL(r)))
}

type requestError struct {
	status  int
	message string
	code    string
}

func (e *requestError) Error() string { return e.message }

func badRequest(status int, code, format string, args ...any) *requestError {
	return &requestError{status: status, code: code, message: fmt.Sprintf(format, args...)}
}

func (s *Server) parseCreateRequest(r *http.Request) (*pipeline.SubmitRequest, error) {
	ct := r.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(ct)

	settings := s.store.Settings()
	source, apiKeyID, apiKeyTag := s.requestIdentity(r)

	if mediaType == "multipart/form-data" {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return nil, badRequest(http.StatusBadRequest, "bad_multipart", "无法解析 multipart 表单: %v", err)
		}
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
		file, header, err := r.FormFile("image")
		if err != nil {
			return nil, badRequest(http.StatusBadRequest, "missing_image", "multipart 请求需要 image 文件字段")
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, settings.ImageMaxBytes+1))
		if err != nil {
			return nil, badRequest(http.StatusBadRequest, "read_image", "读取图片失败: %v", err)
		}
		modelName := firstNonEmpty(r.FormValue("model"), "minimax-h3")
		ratio, err := normalizeRatio(firstNonEmpty(r.FormValue("size"), r.FormValue("ratio")))
		if err != nil {
			return nil, err
		}
		duration, err := normalizeDuration(firstNonEmpty(r.FormValue("seconds"), r.FormValue("duration")), modelName)
		if err != nil {
			return nil, err
		}
		filename := "upload.jpg"
		if header != nil && header.Filename != "" {
			filename = sanitizeFilename(header.Filename)
		}
		return &pipeline.SubmitRequest{
			Image: data, Filename: filename,
			Prompt: strings.TrimSpace(r.FormValue("prompt")),
			Model:  modelName, Ratio: ratio, Duration: duration,
			Source: source, APIKeyID: apiKeyID, APIKeyTag: apiKeyTag,
		}, nil
	}

	var body createVideoBody
	if err := readJSON(r, &body); err != nil {
		return nil, badRequest(http.StatusBadRequest, "bad_json", "请求体必须是 JSON 或 multipart/form-data: %v", err)
	}
	modelName := firstNonEmpty(body.Model, "minimax-h3")
	ratio, err := normalizeRatio(firstNonEmpty(body.Size, body.Ratio))
	if err != nil {
		return nil, err
	}
	duration, err := normalizeDuration(firstNonEmpty(rawToString(body.Seconds), rawToString(body.Duration)), modelName)
	if err != nil {
		return nil, err
	}
	ref := firstNonEmpty(rawToString(body.ImageURL), rawToString(body.Image))
	if ref == "" {
		return nil, badRequest(http.StatusBadRequest, "missing_image",
			"必须提供 image_url 或 image（http(s) 链接或 base64 data URL）：试用通道仅支持图生视频")
	}
	data, filename, err := s.fetchImage(r, ref)
	if err != nil {
		return nil, err
	}
	return &pipeline.SubmitRequest{
		Image: data, Filename: filename,
		Prompt: strings.TrimSpace(body.Prompt),
		Model:  modelName, Ratio: ratio, Duration: duration,
		Source: source, APIKeyID: apiKeyID, APIKeyTag: apiKeyTag,
	}, nil
}

// rawToString unwraps the several shapes a JSON field may take: a bare string, a
// number, or the OpenAI {"url": "..."} object.
func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return strings.TrimSpace(str)
	}
	var num json.Number
	if err := json.Unmarshal(raw, &num); err == nil {
		return num.String()
	}
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return strings.TrimSpace(obj.URL)
	}
	return ""
}

func (s *Server) requestIdentity(r *http.Request) (source, keyID, keyTag string) {
	source = "anonymous"
	if v, ok := r.Context().Value(ctxKeySource).(string); ok && v != "" {
		source = v
	}
	if k, ok := r.Context().Value(ctxKeyAPIKey).(*model.APIKey); ok && k != nil {
		keyID = k.ID
		keyTag = k.Name
	}
	return source, keyID, keyTag
}

func (s *Server) writeSubmitError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pipeline.ErrImageRejected):
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_image")
	case errors.Is(err, pipeline.ErrUpstreamBusy):
		writeOpenAIError(w, http.StatusBadGateway, err.Error(), "upstream_unavailable")
	default:
		var ue *upstream.Error
		if errors.As(err, &ue) {
			status := http.StatusBadGateway
			if ue.Status == 429 {
				status = http.StatusTooManyRequests
			} else if ue.Status >= 400 && ue.Status < 500 {
				status = http.StatusBadRequest
			}
			writeOpenAIError(w, status, ue.Error(), ue.Code)
			return
		}
		writeOpenAIError(w, http.StatusInternalServerError, err.Error(), "internal_error")
	}
}

// fetchImage resolves an image reference to bytes. http(s) URLs and base64 data
// URLs are both supported.
func (s *Server) fetchImage(r *http.Request, ref string) ([]byte, string, error) {
	if m := dataURLRe.FindStringSubmatch(ref); m != nil {
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(m[2]))
		if err != nil {
			// Tolerate unpadded base64, which browsers occasionally emit.
			data, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(m[2]))
			if err != nil {
				return nil, "", badRequest(http.StatusBadRequest, "bad_base64", "data URL 的 base64 内容无效")
			}
		}
		ext := "jpg"
		switch m[1] {
		case "image/png":
			ext = "png"
		case "image/webp":
			ext = "webp"
		}
		return data, "upload." + ext, nil
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		u, err := url.Parse(ref)
		if err != nil {
			return nil, "", badRequest(http.StatusBadRequest, "bad_url", "image_url 不是合法链接")
		}
		ctx, cancel := contextWithTimeout(r, 45*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, "", badRequest(http.StatusBadRequest, "bad_url", "无法构造图片请求: %v", err)
		}
		resp, err := s.imageClient().Do(req)
		if err != nil {
			return nil, "", badRequest(http.StatusBadRequest, "image_fetch_failed", "拉取 image_url 失败: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, "", badRequest(http.StatusBadRequest, "image_fetch_failed",
				"拉取 image_url 失败: HTTP %d", resp.StatusCode)
		}
		limit := s.store.Settings().ImageMaxBytes
		data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return nil, "", badRequest(http.StatusBadRequest, "image_fetch_failed", "读取图片失败: %v", err)
		}
		name := path.Base(u.Path)
		if name == "" || name == "." || name == "/" {
			name = "upload.jpg"
		}
		return data, sanitizeFilename(name), nil
	}
	return nil, "", badRequest(http.StatusBadRequest, "bad_image",
		"image 必须是 http(s) 链接或 base64 data URL")
}

func (s *Server) imageClient() *http.Client {
	s.imageClientOnce.Do(func() {
		s.imageHTTP = &http.Client{Timeout: 60 * time.Second}
	})
	return s.imageHTTP
}

// ---------------------------------------------------------------------------
// GET /v1/videos/{id}
// ---------------------------------------------------------------------------

func (s *Server) handleGetVideo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, ok := s.store.GetTask(id)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "video task not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, taskToVideoObject(t, s.baseURL(r)))
}

// ---------------------------------------------------------------------------
// GET /v1/videos/{id}/content
// ---------------------------------------------------------------------------

func (s *Server) handleVideoContent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, ok := s.store.GetTask(id)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "video task not found", "not_found")
		return
	}
	if t.Status != model.StatusSucceeded {
		writeOpenAIError(w, http.StatusConflict, fmt.Sprintf("task is %s, not ready", t.Status), "not_ready")
		return
	}
	data, media, err := s.pipe.Content(r.Context(), t)
	if err != nil {
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Status == 404 {
			writeOpenAIError(w, http.StatusNotFound, "上游已回收该视频，请重新生成", "upstream_expired")
			return
		}
		writeOpenAIError(w, http.StatusBadGateway, "拉取上游视频失败: "+err.Error(), "upstream_error")
		return
	}
	w.Header().Set("Content-Type", media)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sanitizeFilename(id+".mp4")))
	if r.URL.Query().Get("inline") == "1" {
		w.Header().Set("Content-Disposition", "inline")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------------------
// POST /v1/chat/completions
// ---------------------------------------------------------------------------

type chatBody struct {
	Model       string          `json:"model"`
	Messages    []chatMessage   `json:"messages"`
	Stream      bool            `json:"stream"`
	Video       *chatVideoSpec  `json:"video"`
	Size        string          `json:"size"`
	Seconds     json.RawMessage `json:"seconds"`
	WaitSeconds float64         `json:"wait_seconds"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type chatVideoSpec struct {
	Model       string          `json:"model"`
	Size        string          `json:"size"`
	Ratio       string          `json:"ratio"`
	Seconds     json.RawMessage `json:"seconds"`
	Duration    json.RawMessage `json:"duration"`
	WaitSeconds float64         `json:"wait_seconds"`
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var body chatBody
	if err := readJSON(r, &body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "请求体必须是 JSON: "+err.Error(), "bad_json")
		return
	}
	if len(body.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "messages[] 不能为空", "bad_request")
		return
	}

	msg, ok := lastUserMessage(body.Messages)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "messages[] 里没有 user 消息", "bad_request")
		return
	}
	imageRef, text := extractLastUserContent(msg)
	if imageRef == "" {
		writeOpenAIError(w, http.StatusBadRequest, "最后一条 user 消息里没有图片；"+
			"MiniMax-H3 试用通道仅支持图生视频（请附带 image_url part）", "missing_image")
		return
	}

	modelName := firstNonEmpty(body.Model, videoField(body.Video, func(v *chatVideoSpec) string { return v.Model }), "minimax-h3")
	ratio, err := normalizeRatio(firstNonEmpty(
		videoField(body.Video, func(v *chatVideoSpec) string { return firstNonEmpty(v.Size, v.Ratio) }),
		body.Size))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "bad_request")
		return
	}
	seconds := firstNonEmpty(
		rawToString(videoFieldRaw(body.Video, func(v *chatVideoSpec) json.RawMessage { return v.Seconds })),
		rawToString(videoFieldRaw(body.Video, func(v *chatVideoSpec) json.RawMessage { return v.Duration })),
		rawToString(body.Seconds))
	duration, err := normalizeDuration(seconds, modelName)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "bad_request")
		return
	}

	data, filename, err := s.fetchImage(r, imageRef)
	if err != nil {
		var bad *requestError
		if errors.As(err, &bad) {
			writeOpenAIError(w, bad.status, bad.message, bad.code)
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "bad_request")
		return
	}

	source, keyID, keyTag := s.requestIdentity(r)
	task, err := s.pipe.Submit(r.Context(), pipeline.SubmitRequest{
		Image: data, Filename: filename, Prompt: text,
		Model: modelName, Ratio: ratio, Duration: duration,
		Source: source, APIKeyID: keyID, APIKeyTag: keyTag,
	})
	if err != nil {
		s.writeSubmitError(w, err)
		return
	}

	base := s.baseURL(r)
	contentURL := base + "/v1/videos/" + task.ID + "/content"
	waitSeconds := body.WaitSeconds
	if waitSeconds == 0 && body.Video != nil {
		waitSeconds = body.Video.WaitSeconds
	}

	message := fmt.Sprintf("Video generation submitted.\n\n- task: `%s` (status: queued, %ds, %s)\n- download: %s\n- poll: GET %s/v1/videos/%s",
		task.ID, duration, ratio, contentURL, base, task.ID)

	if waitSeconds > 0 {
		deadline := time.Now().Add(time.Duration(waitSeconds * float64(time.Second)))
		for time.Now().Before(deadline) {
			select {
			case <-r.Context().Done():
			case <-time.After(s.store.Settings().PollInterval()):
			}
			cur, ok := s.store.GetTask(task.ID)
			if !ok {
				break
			}
			task = cur
			if task.Terminal() {
				break
			}
		}
		switch task.Status {
		case model.StatusSucceeded:
			message = fmt.Sprintf("Video ready.\n\n- download: %s\n- task: `%s`", contentURL, task.ID)
		case model.StatusFailed:
			message = "Video generation failed: " + task.Error
		default:
			message = fmt.Sprintf("Still %s after %.0fs.\n\n- task: `%s`\n- download when ready: %s",
				task.Status, waitSeconds, task.ID, contentURL)
		}
	}

	if body.Stream {
		s.streamChat(w, r, modelName, message)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-" + task.ID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": message},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		"video": taskToVideoObject(task, base),
	})
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, modelName, message string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming unsupported", "internal_error")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	writeChunk := func(delta map[string]any, finish any) {
		payload, _ := json.Marshal(map[string]any{
			"id":      "chatcmpl-stream",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   modelName,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		flusher.Flush()
	}
	writeChunk(map[string]any{"role": "assistant"}, nil)
	for _, line := range strings.Split(message, "\n") {
		writeChunk(map[string]any{"content": line + "\n"}, nil)
	}
	writeChunk(map[string]any{}, "stop")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func extractLastUserContent(msg chatMessage) (imageRef, text string) {
	if len(msg.Content) == 0 {
		return "", ""
	}
	var plain string
	if err := json.Unmarshal(msg.Content, &plain); err == nil {
		return "", strings.TrimSpace(plain)
	}
	var parts []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if err := json.Unmarshal(msg.Content, &parts); err != nil {
		return "", ""
	}
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			if t := strings.TrimSpace(p.Text); t != "" {
				texts = append(texts, t)
			}
		case "image_url":
			if imageRef == "" {
				imageRef = rawToString(p.ImageURL)
			}
		}
	}
	return imageRef, strings.Join(texts, " ")
}

// lastUserMessage returns the final user turn, which is where clients put the
// image they want animated.
func lastUserMessage(messages []chatMessage) (chatMessage, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			return messages[i], true
		}
	}
	return chatMessage{}, false
}

// ---------------------------------------------------------------------------
// normalisation
// ---------------------------------------------------------------------------

// normalizeRatio enforces the upstream limitation: the trial channel only emits
// 9:16 vertical video.
func normalizeRatio(value string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return "9:16", nil
	}
	switch v {
	case "9:16", "720x1280", "1080x1920", "vertical", "portrait":
		return "9:16", nil
	}
	return "", badRequest(http.StatusBadRequest, "unsupported_ratio",
		"试用通道仅支持 9:16 竖屏输出（上游硬限制），收到 %q", value)
}

func normalizeDuration(value, modelName string) (int, error) {
	switch {
	case strings.HasSuffix(modelName, "-10s"):
		return 10, nil
	case strings.HasSuffix(modelName, "-15s"):
		return 15, nil
	}
	v := strings.TrimSpace(value)
	if v == "" {
		return 6, nil
	}
	secs, err := strconv.Atoi(strings.TrimSuffix(v, "s"))
	if err != nil {
		f, ferr := strconv.ParseFloat(v, 64)
		if ferr != nil {
			return 0, badRequest(http.StatusBadRequest, "bad_duration", "无效的时长 %q", value)
		}
		secs = int(f)
	}
	mapped, ok := durationAliases[secs]
	if !ok {
		return 0, badRequest(http.StatusBadRequest, "unsupported_duration",
			"不支持的时长 %ds；试用通道支持 6s / 10s / 15s", secs)
	}
	return mapped, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func videoField(v *chatVideoSpec, fn func(*chatVideoSpec) string) string {
	if v == nil {
		return ""
	}
	return fn(v)
}

func videoFieldRaw(v *chatVideoSpec, fn func(*chatVideoSpec) json.RawMessage) json.RawMessage {
	if v == nil {
		return nil
	}
	return fn(v)
}

// lookupTask is a small helper used by the admin handlers too.
func (s *Server) lookupTask(id string) (*model.Task, error) {
	t, ok := s.store.GetTask(id)
	if !ok {
		return nil, store.ErrNotFound
	}
	return t, nil
}
