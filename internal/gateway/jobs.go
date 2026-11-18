package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"suno2api/internal/generate"
)

type generationInput struct {
	generate.Request
	AccountID string `json:"account_id,omitempty"`
}

func normalizeGeneration(request generate.Request, defaultModel string) (generate.Request, error) {
	if strings.TrimSpace(request.Model) == "" {
		request.Model = defaultModel
	}
	if request.BatchSize == 0 {
		request.BatchSize = 2
	}
	if request.BatchSize < 1 || request.BatchSize > 2 {
		return request, errors.New("batch_size 只能是 1 或 2")
	}
	description := strings.TrimSpace(request.Prompt)
	lyrics := strings.TrimSpace(request.Lyrics)
	style := strings.TrimSpace(request.Style)
	if len(request.Prompt)+len(request.Lyrics) > 65536 {
		return request, errors.New("歌曲文本过长")
	}
	custom := request.Custom || lyrics != ""
	if !custom && request.Instrumental && description == "" && style != "" {
		custom = true
	}
	request.Custom = custom
	if custom && request.Instrumental {
		// Instrumental songs have no vocal track: never forward the text as lyrics.
		request.Lyrics = ""
		request.Prompt = ""
	}
	if custom {
		if request.Instrumental {
			if style == "" {
				return request, errors.New("纯音乐自定义模式需要填写音乐风格")
			}
		} else if lyrics == "" {
			return request, errors.New("自定义模式需要歌词；简单描述请关闭自定义模式")
		}
	} else if description == "" {
		return request, errors.New("请填写音乐描述")
	}
	return request, nil
}

func (gateway *Gateway) pickAccountLocked(explicit, model string) (string, *runtimeAccount, error) {
	if gateway.closed {
		return "", nil, errors.New("gateway is closed")
	}
	var selected string
	var chosen *runtimeAccount
	ids := make([]string, 0, len(gateway.state.Accounts))
	for id := range gateway.state.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if explicit != "" && id != explicit {
			continue
		}
		record := gateway.state.Accounts[id]
		runtime := gateway.accounts[id]
		if !record.Enabled || record.DeletedAt != nil || runtime == nil || record.Inflight >= record.MaxConcurrency {
			continue
		}
		if record.Session != nil && record.Session.ExpiresAt.Before(time.Now()) && (!record.Session.CanRefresh || record.Session.ReauthenticationRequired || record.Session.State == "verification_required" || record.Session.State == "refresh_blocked") {
			continue
		}
		if record.State == "configuration_error" || record.Session != nil && record.Session.ExpiresAt.IsZero() {
			continue
		}
		if record.Billing != nil && model != "" {
			key := model
			if key == "v6-mini" {
				key = "chirp-goose"
			}
			allowed := false
			for _, candidate := range record.Billing.Models {
				if candidate.ExternalKey == key && candidate.CanUse {
					allowed = true
				}
			}
			if len(record.Billing.Models) > 0 && !allowed {
				continue
			}
		}
		if chosen == nil || runtime.cursor < chosen.cursor {
			chosen = runtime
			selected = id
		}
	}
	if chosen == nil {
		return "", nil, errors.New("no available account")
	}
	chosen.cursor += 1 / float64(gateway.state.Accounts[selected].Weight)
	chosen.inflight++
	gateway.state.Accounts[selected].Inflight++
	return selected, chosen, nil
}

// startJob selects an account, records the job before any upstream request, and
// returns the runtime that must submit it.
func (gateway *Gateway) startJob(accountID, model string) (*JobView, *runtimeAccount, error) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	id, runtime, err := gateway.pickAccountLocked(accountID, model)
	if err != nil {
		gateway.logf("warn", "job", "没有可用账号接收任务", map[string]any{"model": model, "requested_account": accountID})
		return nil, nil, err
	}
	release := func() {
		if runtime.inflight > 0 {
			runtime.inflight--
		}
		if account := gateway.state.Accounts[id]; account != nil && account.Inflight > 0 {
			account.Inflight--
		}
	}
	jobID, err := randomID()
	if err != nil {
		release()
		return nil, nil, err
	}
	gateway.logf("info", "job", "任务已创建，等待提交", map[string]any{"account": gateway.accountNameLocked(id), "model": model})
	now := time.Now().UTC()
	job := &JobView{ID: jobID, AccountID: id, RequestedModel: model, State: "submitting", ClipIDs: []string{}, Clips: []ClipView{}, CreatedAt: now, UpdatedAt: now}
	gateway.state.Jobs[jobID] = job
	if err := gateway.persistJobLocked(jobID); err != nil {
		delete(gateway.state.Jobs, jobID)
		release()
		return nil, nil, errors.New("job record could not be saved")
	}
	snapshot := *job
	return &snapshot, runtime, nil
}

func (gateway *Gateway) applyJobOutcomeLocked(job *JobView, response generate.Response, err error) {
	job.RequestID = response.ID
	job.ClipIDs = response.IDs()
	job.RequestedIDs = response.RequestedIDs()
	if len(job.RequestedIDs) == 0 && response.RequestedModel == "" {
		wanted := job.RequestedModel
		if wanted == "v6-mini" {
			wanted = "chirp-goose"
		}
		for _, clip := range response.Clips {
			if clip.ModelName == wanted || clip.ModelName == "" {
				job.RequestedIDs = append(job.RequestedIDs, clip.ID)
			}
		}
	}
	job.Clips = projectClips(response.Clips)
	job.UpdatedAt = time.Now().UTC()
	if err == nil {
		job.State = jobState(job)
		if job.State == "submitted" {
			job.Error = ""
		}
		return
	}
	_, _, message := operationError(err)
	job.Error = message
	job.State = "failed"
	var network net.Error
	if response.ID != "" || len(response.Clips) > 0 {
		job.State = "submitted"
		job.Error = "上游已返回任务，但响应需核查；请查询原任务，不要重提"
	} else if errors.As(err, &network) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ambiguousGenerationError(err) {
		job.State = "submission_unknown"
		job.Error = "网络中断，提交结果未确认；请在原账号作品中核对，不会自动换号重提"
	}
}

// executeJob submits one job and always releases its account slot.
func (gateway *Gateway) executeJob(parent context.Context, id string, runtime *runtimeAccount, input generate.Request) (JobView, generate.Response, error) {
	defer gateway.releaseAccountForJob(id, runtime)
	ctx, cancel := accountContext(parent, runtime, gateway.config.RequestTimeout)
	defer cancel()
	response, err := runtime.client.Generate(ctx, input)
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	job := gateway.state.Jobs[id]
	if job == nil {
		return JobView{}, response, err
	}
	previous := job.State
	gateway.applyJobOutcomeLocked(job, response, err)
	_ = gateway.persistJobLocked(id)
	gateway.logJobOutcomeLocked(job, previous, err)
	return *job, response, err
}

// logJobOutcomeLocked records state transitions once, so 15-second polling
// does not flood the log with repeated "submitted" rows.
func (gateway *Gateway) logJobOutcomeLocked(job *JobView, previous string, err error) {
	if job.State == previous {
		return
	}
	fields := map[string]any{"job": job.ID, "account": gateway.accountNameLocked(job.AccountID), "from": previous, "to": job.State}
	switch job.State {
	case "complete":
		gateway.logf("info", "job", "任务完成", fields)
	case "failed":
		if err != nil {
			fields["error"] = operationErrorMessage(err)
		} else {
			fields["error"] = job.Error
		}
		gateway.logf("error", "job", "任务失败", fields)
	case "submission_unknown":
		gateway.logf("warn", "job", "提交结果未确认", fields)
	case "submitted":
		if previous == "submitting" {
			gateway.logf("info", "job", "上游已受理生成请求", fields)
		}
	}
}

func (gateway *Gateway) releaseAccountForJob(jobID string, runtime *runtimeAccount) {
	gateway.mu.Lock()
	accountID := ""
	if job := gateway.state.Jobs[jobID]; job != nil {
		accountID = job.AccountID
	}
	gateway.mu.Unlock()
	if accountID != "" {
		gateway.releaseAccount(accountID, runtime)
	}
}

func (gateway *Gateway) submitJob(id string, runtime *runtimeAccount, input generate.Request) {
	defer gateway.wait.Done()
	// Keep the submission alive if the HTTP caller disconnects; the job record
	// already exists, so a dropped response must never cause an automatic retry.
	gateway.executeJob(runtime.ctx, id, runtime, input)
}

func (gateway *Gateway) pollJobs(ctx context.Context) {
	gateway.mu.Lock()
	ids := []string{}
	for id, job := range gateway.state.Jobs {
		if job.State == "submitted" && len(job.ClipIDs) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > 20 {
		start := gateway.pollCursor % len(ids)
		selected := make([]string, 0, 20)
		for index := 0; index < 20; index++ {
			selected = append(selected, ids[(start+index)%len(ids)])
		}
		gateway.pollCursor = (start + 20) % len(ids)
		ids = selected
	}
	gateway.mu.Unlock()
	for _, id := range ids {
		if ctx.Err() != nil || gateway.ctx.Err() != nil {
			return
		}
		_, _ = gateway.refreshJob(ctx, id)
	}
}

func (gateway *Gateway) refreshJob(parent context.Context, id string) (JobView, error) {
	gateway.mu.Lock()
	job := gateway.state.Jobs[id]
	if job == nil {
		gateway.mu.Unlock()
		return JobView{}, errors.New("job missing")
	}
	snapshot := *job
	runtime := gateway.accounts[job.AccountID]
	gateway.mu.Unlock()
	if runtime == nil || len(snapshot.ClipIDs) == 0 || (snapshot.State != "submitted" && snapshot.State != "submission_unknown") {
		return snapshot, nil
	}
	ctx, cancel := accountContext(parent, runtime, time.Minute)
	defer cancel()
	ids := snapshot.RequestedIDs
	if len(ids) == 0 {
		ids = snapshot.ClipIDs
	}
	clips, err := runtime.client.Get(ctx, ids)
	if err != nil {
		return snapshot, err
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	current := gateway.state.Jobs[id]
	if current == nil || gateway.accounts[current.AccountID] != runtime {
		return snapshot, errors.New("account changed")
	}
	current.Clips = projectClips(clips)
	current.State = jobState(current)
	current.UpdatedAt = time.Now().UTC()
	current.Error = ""
	if err := gateway.persistJobLocked(id); err != nil {
		return *current, err
	}
	return *current, nil
}

func (gateway *Gateway) releaseAccount(id string, runtime *runtimeAccount) {
	gateway.mu.Lock()
	if runtime.inflight > 0 {
		runtime.inflight--
	}
	if account := gateway.state.Accounts[id]; account != nil && account.Inflight > 0 {
		account.Inflight--
	}
	gateway.mu.Unlock()
}

func (gateway *Gateway) generateHTTP(writer http.ResponseWriter, request *http.Request) {
	var input generationInput
	if !decodeJSON(writer, request, &input) {
		return
	}
	normalized, err := normalizeGeneration(input.Request, gateway.config.DefaultModel)
	if err != nil {
		writeError(writer, 400, "invalid_request", err.Error())
		return
	}
	job, runtime, err := gateway.startJob(input.AccountID, normalized.Model)
	if err != nil {
		writeError(writer, 503, "no_available_account", "没有可用账号，或已达到账号并发上限")
		return
	}
	gateway.wait.Add(1)
	go gateway.submitJob(job.ID, runtime, normalized)
	writeJSON(writer, http.StatusAccepted, map[string]any{"job": *job})
}

func operationErrorMessage(err error) string {
	_, _, message := operationError(err)
	return message
}

func ambiguousGenerationError(err error) bool {
	var apiError *generate.APIError
	return !errors.As(err, &apiError) && !errors.Is(err, generate.ErrAuthentication) && !errors.Is(err, generate.ErrCaptchaRequired) && !errors.Is(err, generate.ErrInsufficientCredits) && !errors.Is(err, generate.ErrRateLimited)
}

func projectClips(clips []generate.Clip) []ClipView {
	result := make([]ClipView, 0, len(clips))
	for _, clip := range clips {
		result = append(result, ClipView{ID: clip.ID, Title: clip.Title, Status: clip.Status, Model: clip.ModelName, MediaReady: clip.AudioURL != "" || len(clip.MediaURLs) > 0})
	}
	return result
}
func clipState(clips []ClipView) string {
	if len(clips) == 0 {
		return "submitted"
	}
	complete := true
	for _, clip := range clips {
		if clip.Status == "error" || clip.Status == "failed" {
			return "failed"
		}
		if clip.Status != "complete" || !clip.MediaReady {
			complete = false
		}
	}
	if complete {
		return "complete"
	}
	return "submitted"
}

func jobState(job *JobView) string {
	if len(job.RequestedIDs) == 0 {
		if job.RequestedModel != "" && len(job.Clips) > 0 {
			return "no_requested_model"
		}
		return clipState(job.Clips)
	}
	wanted := map[string]bool{}
	for _, id := range job.RequestedIDs {
		wanted[id] = true
	}
	var clips []ClipView
	for _, clip := range job.Clips {
		if wanted[clip.ID] {
			clips = append(clips, clip)
		}
	}
	if len(clips) != len(wanted) {
		return "submitted"
	}
	return clipState(clips)
}
func (gateway *Gateway) jobViewsLocked() []JobView {
	views := make([]JobView, 0, len(gateway.state.Jobs))
	for _, job := range gateway.state.Jobs {
		views = append(views, *job)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].CreatedAt.After(views[j].CreatedAt) })
	return views
}
func (gateway *Gateway) listJobs(writer http.ResponseWriter, request *http.Request) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	writeJSON(writer, 200, map[string]any{"jobs": gateway.jobViewsLocked()})
}
func (gateway *Gateway) getJobHTTP(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	if id == "list" {
		gateway.listJobs(writer, request)
		return
	}
	snapshot, err := gateway.refreshJob(request.Context(), id)
	if snapshot.ID == "" && err != nil {
		writeError(writer, 404, "not_found", "任务不存在")
		return
	}
	writeJSON(writer, 200, map[string]any{"job": snapshot})
}
func (gateway *Gateway) songsHTTP(writer http.ResponseWriter, request *http.Request) {
	accountID := request.URL.Query().Get("account_id")
	if accountID == "" {
		writeError(writer, 400, "account_required", "查询歌曲须指定原account_id")
		return
	}
	ids := strings.Split(request.URL.Query().Get("ids"), ",")
	if len(ids) == 0 || len(ids) > 20 || ids[0] == "" {
		writeError(writer, 400, "ids_required", "请提供1至20个歌曲ID")
		return
	}
	runtime, err := gateway.accountForOperation(accountID)
	if err != nil {
		writeError(writer, 409, "account_unavailable", "原账号不可用或已归档")
		return
	}
	ctx, cancel := accountContext(request.Context(), runtime, time.Minute)
	defer cancel()
	clips, err := runtime.client.Get(ctx, ids)
	if err != nil {
		status, code, message := operationError(err)
		writeError(writer, status, code, message)
		return
	}
	writeJSON(writer, 200, map[string]any{"account_id": accountID, "clips": projectClips(clips)})
}
