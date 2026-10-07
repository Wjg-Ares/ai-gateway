package admin

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wjg-Ares/ai-gateway/internal/codexauth"
	"github.com/Wjg-Ares/ai-gateway/internal/database"
)

type session struct {
	user    database.UserRecord
	expires time.Time
}

type oauthFlow struct {
	auth        codexauth.AuthorizationRequest
	modelIDs    []int64
	ownerUserID int64
	accountName string
	expires     time.Time
	bundle      *codexauth.TokenBundle
	errText     string
}

// Handler serves the built-in login page and role-scoped administration API.
// It deliberately uses only the Go standard library so the gateway remains a
// single binary with no frontend build step.
type Handler struct {
	store            *database.Store
	encryptionSecret string
	providers        []string
	mu               sync.RWMutex
	sessions         map[string]session
	oauthFlows       map[string]oauthFlow
	oauthHostID      string
}

func New(store *database.Store, encryptionSecret string, providers []string) http.Handler {
	hostID, _ := randomHostID()
	return &Handler{store: store, encryptionSecret: encryptionSecret, providers: providers, sessions: make(map[string]session), oauthFlows: make(map[string]oauthFlow), oauthHostID: hostID}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin/" || r.URL.Path == "/admin" {
		writeHTML(w, adminHTML)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/admin/api/login" {
		h.login(w, r)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/admin/api/logout" {
		h.logout(w, r)
		return
	}
	if r.Method == http.MethodPost && (r.URL.Path == "/admin/api/codex/start" || r.URL.Path == "/admin/api/codex/poll") {
		user, ok := h.currentUser(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
			return
		}
		if r.URL.Path == "/admin/api/codex/start" {
			h.startCodexFlow(w, r, user)
		} else {
			h.pollCodexFlow(w, r)
		}
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/admin/api/codex/callback" {
		h.codexCallback(w, r)
		return
	}
	user, ok := h.currentUser(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/admin/api/session":
		writeJSON(w, http.StatusOK, user)
	case r.Method == http.MethodGet && r.URL.Path == "/admin/api/users":
		if !requireAdmin(w, user) {
			return
		}
		items, err := h.store.ListUsers(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case r.Method == http.MethodPost && r.URL.Path == "/admin/api/users":
		if !requireAdmin(w, user) {
			return
		}
		var input struct {
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
			Role        string `json:"role"`
			Password    string `json:"password"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		item, err := h.store.CreateUser(r.Context(), input.Username, input.DisplayName, input.Role, input.Password)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, item)
	case r.Method == http.MethodPost && r.URL.Path == "/admin/api/users/password":
		if !requireAdmin(w, user) {
			return
		}
		var input struct {
			UserID   int64  `json:"user_id"`
			Password string `json:"password"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := h.store.SetUserPassword(r.Context(), input.UserID, input.Password); err != nil {
			if errors.Is(err, database.ErrProtectedAdmin) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "password_updated"})
	case r.Method == http.MethodPost && r.URL.Path == "/admin/api/users/delete":
		if !requireAdmin(w, user) {
			return
		}
		var input struct {
			UserID int64 `json:"user_id"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := h.store.DeleteUser(r.Context(), input.UserID); err != nil {
			if errors.Is(err, database.ErrProtectedAdmin) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	case r.Method == http.MethodGet && r.URL.Path == "/admin/api/models":
		items, err := h.store.ListModelsAdmin(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case r.Method == http.MethodGet && r.URL.Path == "/admin/api/providers":
		writeJSON(w, http.StatusOK, h.providers)
	case r.Method == http.MethodPost && r.URL.Path == "/admin/api/models":
		if !requireAdmin(w, user) {
			return
		}
		var input struct {
			Name          string `json:"name"`
			Provider      string `json:"provider"`
			UpstreamModel string `json:"upstream_model"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		item, err := h.store.CreateModel(r.Context(), input.Name, input.Provider, input.UpstreamModel)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, item)
	case r.Method == http.MethodPost && r.URL.Path == "/admin/api/models/delete":
		if !requireAdmin(w, user) {
			return
		}
		var input struct {
			ModelID int64 `json:"model_id"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := h.store.DeleteModel(r.Context(), input.ModelID); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	case r.Method == http.MethodGet && r.URL.Path == "/admin/api/model-keys":
		modelIDs, err := parseModelIDs(r.URL.Query().Get("model_ids"), r.URL.Query().Get("model_id"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one model_id is required"})
			return
		}
		var items []database.ModelKeyRecord
		if user.Role == "admin" {
			items, err = h.store.ListModelKeysForModels(r.Context(), modelIDs, 0, true)
		} else {
			items, err = h.store.ListModelKeysForModels(r.Context(), modelIDs, user.ID, false)
		}
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case r.Method == http.MethodPost && r.URL.Path == "/admin/api/model-keys":
		var input struct {
			ModelID        int64   `json:"model_id"`
			ModelIDs       []int64 `json:"model_ids"`
			AccountName    string  `json:"account_name"`
			APIKey         string  `json:"api_key"`
			Priority       int     `json:"priority"`
			MaxConcurrency int     `json:"max_concurrency"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		modelIDs := input.ModelIDs
		if len(modelIDs) == 0 {
			modelIDs = []int64{input.ModelID}
		}
		item, err := h.store.CreateModelKey(r.Context(), modelIDs, user.ID, input.AccountName, input.APIKey, input.Priority, input.MaxConcurrency, h.encryptionSecret)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, item)
	case r.Method == http.MethodPost && r.URL.Path == "/admin/api/model-keys/delete":
		var input struct {
			ModelKeyID int64 `json:"model_key_id"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := h.store.DeleteModelKey(r.Context(), input.ModelKeyID, user.ID, user.Role == "admin"); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	case r.Method == http.MethodPost && r.URL.Path == "/admin/api/assignments":
		if !requireAdmin(w, user) {
			return
		}
		var input struct {
			UserID     int64 `json:"user_id"`
			ModelKeyID int64 `json:"model_key_id"`
			Default    bool  `json:"default"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if input.UserID <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请选择要分配给的用户"})
			return
		}
		if input.ModelKeyID <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请选择要分配的模型 Key"})
			return
		}
		if err := h.store.AssignSharedModelKey(r.Context(), input.UserID, input.ModelKeyID, input.Default); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "assigned"})
	case r.Method == http.MethodGet && r.URL.Path == "/admin/api/assignments":
		if !requireAdmin(w, user) {
			return
		}
		items, err := h.store.ListSharedModelKeys(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case r.Method == http.MethodPost && r.URL.Path == "/admin/api/assignments/delete":
		if !requireAdmin(w, user) {
			return
		}
		var input struct {
			UserID     int64 `json:"user_id"`
			ModelKeyID int64 `json:"model_key_id"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if input.UserID <= 0 || input.ModelKeyID <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请选择有效的用户和模型 Key"})
			return
		}
		if err := h.store.DeleteSharedModelKey(r.Context(), input.UserID, input.ModelKeyID); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	case r.Method == http.MethodPost && r.URL.Path == "/admin/api/gateway-keys":
		if !requireAdmin(w, user) {
			return
		}
		var input struct {
			UserID int64  `json:"user_id"`
			Name   string `json:"name"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		key, err := h.store.CreateGatewayKey(r.Context(), input.UserID, input.Name)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"key": key, "warning": "copy this key now; it is not shown again"})
	case r.Method == http.MethodGet && r.URL.Path == "/admin/api/gateway-keys":
		if !requireAdmin(w, user) {
			return
		}
		items, err := h.store.ListGatewayKeys(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case r.Method == http.MethodPost && r.URL.Path == "/admin/api/gateway-keys/delete":
		if !requireAdmin(w, user) {
			return
		}
		var input struct {
			KeyID int64 `json:"key_id"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := h.store.DeleteGatewayKey(r.Context(), input.KeyID); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "admin endpoint not found"})
	}
}

func (h *Handler) startCodexFlow(w http.ResponseWriter, r *http.Request, user database.UserRecord) {
	var input struct {
		ModelID     int64   `json:"model_id"`
		ModelIDs    []int64 `json:"model_ids"`
		AccountName string  `json:"account_name"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	modelIDs := input.ModelIDs
	if len(modelIDs) == 0 && input.ModelID > 0 {
		modelIDs = []int64{input.ModelID}
	}
	if len(modelIDs) == 0 || strings.TrimSpace(input.AccountName) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one model and an account name are required"})
		return
	}
	redirectURI, err := loopbackRedirectURI(r.Host)
	if err != nil {
		writeError(w, err)
		return
	}
	auth, err := codexauth.StartAuthorization(r.Context(), redirectURI, h.oauthHostID)
	if err != nil {
		writeError(w, err)
		return
	}
	flowToken, err := randomToken()
	if err != nil {
		writeError(w, err)
		return
	}
	h.mu.Lock()
	h.oauthFlows[flowToken] = oauthFlow{auth: auth, modelIDs: modelIDs, ownerUserID: user.ID, accountName: input.AccountName, expires: time.Now().Add(10 * time.Minute)}
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"flow_token": flowToken, "authorization_url": auth.URL, "interval": 2, "expires_in": 600})
}

func (h *Handler) pollCodexFlow(w http.ResponseWriter, r *http.Request) {
	var input struct {
		FlowToken string `json:"flow_token"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	h.mu.RLock()
	flow, ok := h.oauthFlows[input.FlowToken]
	h.mu.RUnlock()
	if !ok || time.Now().After(flow.expires) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "login flow expired"})
		return
	}
	if flow.errText != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": flow.errText})
		return
	}
	if flow.bundle == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
		return
	}
	item, err := h.store.CreateCodexOAuthKey(r.Context(), flow.modelIDs, flow.ownerUserID, flow.accountName, *flow.bundle, h.encryptionSecret)
	if err != nil {
		writeError(w, err)
		return
	}
	h.mu.Lock()
	delete(h.oauthFlows, input.FlowToken)
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"status": "complete", "key": item})
}

func (h *Handler) codexCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	h.mu.RLock()
	flowToken := ""
	var flow oauthFlow
	for token, candidate := range h.oauthFlows {
		if candidate.auth.State == state {
			flowToken, flow = token, candidate
			break
		}
	}
	h.mu.RUnlock()
	if flowToken == "" || time.Now().After(flow.expires) {
		writeHTML(w, "<h2>AI Gateway OAuth 回调已失效</h2><p>请回到管理页重新点击 ChatGPT/Codex 登录。</p>")
		return
	}
	callback := codexauth.Callback{Code: r.URL.Query().Get("code"), State: state, ClientID: r.URL.Query().Get("client_id"), Scope: r.URL.Query().Get("scope"), Error: r.URL.Query().Get("error"), ErrorDescription: r.URL.Query().Get("error_description")}
	bundle, err := codexauth.ExchangeCallback(r.Context(), callback, flow.auth)
	h.mu.Lock()
	updated := h.oauthFlows[flowToken]
	if err != nil {
		updated.errText = err.Error()
	} else {
		updated.bundle = &bundle
	}
	h.oauthFlows[flowToken] = updated
	h.mu.Unlock()
	if err != nil {
		writeHTML(w, "<h2>ChatGPT 授权失败</h2><p>"+html.EscapeString(err.Error())+"</p><p>可以关闭此页并回管理页重试。</p>")
		return
	}
	writeHTML(w, "<h2>ChatGPT 授权完成</h2><p>可以关闭此页，回到 AI Gateway 管理页等待绑定完成。</p>")
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	user, err := h.store.AuthenticateUser(r.Context(), input.Username, input.Password)
	if err != nil {
		if errors.Is(err, database.ErrInvalidCredentials) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid username or password"})
			return
		}
		writeError(w, err)
		return
	}
	token, err := randomToken()
	if err != nil {
		writeError(w, err)
		return
	}
	h.mu.Lock()
	h.sessions[token] = session{user: user, expires: time.Now().Add(12 * time.Hour)}
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "ai_gateway_session", Value: token, Path: "/admin/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 12 * 60 * 60})
	writeJSON(w, http.StatusOK, user)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("ai_gateway_session"); err == nil {
		h.mu.Lock()
		delete(h.sessions, cookie.Value)
		h.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "ai_gateway_session", Value: "", Path: "/admin/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

func (h *Handler) currentUser(r *http.Request) (database.UserRecord, bool) {
	cookie, err := r.Cookie("ai_gateway_session")
	if err != nil || cookie.Value == "" {
		return database.UserRecord{}, false
	}
	h.mu.RLock()
	item, ok := h.sessions[cookie.Value]
	h.mu.RUnlock()
	if !ok || time.Now().After(item.expires) {
		return database.UserRecord{}, false
	}
	return item.user, true
}

func requireAdmin(w http.ResponseWriter, user database.UserRecord) bool {
	if user.Role == "admin" {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "administrator role required"})
	return false
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func randomHostID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("urn:uuid:%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func loopbackRedirectURI(hostport string) (string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || port == "" {
		return "", errors.New("请用 127.0.0.1:<端口> 打开管理页，OAuth 回调需要本机回环地址")
	}
	return "http://127.0.0.1:" + port + "/admin/api/codex/callback", nil
}

func parseModelIDs(values ...string) ([]int64, error) {
	seen := map[int64]bool{}
	var result []int64
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			id, err := strconv.ParseInt(item, 10, 64)
			if err != nil || id <= 0 {
				return nil, errors.New("invalid model id")
			}
			if !seen[id] {
				seen[id] = true
				result = append(result, id)
			}
		}
	}
	if len(result) == 0 {
		return nil, errors.New("at least one model id is required")
	}
	return result, nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeHTML(w http.ResponseWriter, value string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(value))
}

const adminHTML = `<!doctype html>
<html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>AI Gateway 管理</title><style>
body{font:14px system-ui,sans-serif;max-width:1100px;margin:24px auto;padding:0 16px;background:#f5f7fb;color:#172033}h1{font-size:24px}h2{margin-top:0}section,.card{background:#fff;border:1px solid #dce2ec;border-radius:10px;padding:16px;margin:14px 0}input,select,button{padding:8px;margin:4px 4px 4px 0;border:1px solid #c4ccda;border-radius:6px}button{cursor:pointer;background:#1f6feb;color:#fff;border:0}.row{display:flex;flex-wrap:wrap;gap:6px}table{border-collapse:collapse;width:100%;margin-top:10px}th,td{border-bottom:1px solid #e7ebf2;text-align:left;padding:7px}code{background:#eef2f7;padding:2px 4px;border-radius:4px}.notice{white-space:pre-wrap;color:#8a3b12}.help{background:#eef6ff;border:1px solid #b9d7ff;border-radius:8px;padding:10px;line-height:1.7;margin:8px 0 12px}.help b{color:#1456a0}.muted{color:#5d6b82}.hidden{display:none}.danger{background:#b42318}
</style>
<div id="login" class="card"><h1>AI Gateway 登录</h1><p>管理员可以管理全部账户和模型；普通用户只能维护自己提供的上游 API Key。</p><input id="loginUser" placeholder="账户，例如 admin"><input id="loginPassword" type="password" placeholder="密码"><button onclick="login()">登录</button><div id="loginMsg" class="notice"></div></div>
<main id="app" class="hidden"><div class="row"><h1 style="margin-right:auto">AI Gateway 管理面板</h1><span id="who"></span><button class="danger" onclick="logout()">退出</button></div>
<section id="usersSection" class="admin-only"><h2>账户管理</h2><div class="row"><input id="username" placeholder="用户名"><input id="displayName" placeholder="显示名"><input id="userPassword" type="password" placeholder="初始密码"><select id="role"><option>user</option><option>operator</option><option>admin</option></select><button onclick="createUser()">创建用户</button></div><table><thead><tr><th>ID</th><th>用户名</th><th>显示名</th><th>角色</th><th>共享 Key</th><th>密码/删除</th></tr></thead><tbody id="users"></tbody></table></section>
<section><h2>模型</h2><div id="modelForm" class="row admin-only"><input id="modelName" placeholder="公开模型名"><select id="provider"></select><input id="upstreamModel" placeholder="上游模型名"><button onclick="createModel()">添加模型</button></div><table><thead><tr><th>ID</th><th>名称</th><th>Provider</th><th>上游模型</th><th>操作</th></tr></thead><tbody id="models"></tbody></table></section>
<section><h2>我的/可用模型 Key</h2><div class="help"><b>要让 Claude 使用 ChatGPT：</b><br>① 模型只选 provider 为 <code>openai-direct</code> 的模型（例如 gpt-6-sol）；<br>② 账户名随便填，例如 <code>ChatGPT OAuth</code>；<br>③ 点击“开始 ChatGPT 授权”，在新页面登录并同意一次；<br>④ 授权页面显示完成后，回到这里等待“绑定成功”。<br><span class="muted">模型框支持多选：单选直接点击，多选按住 Ctrl 再点。第一次建议只选一个 openai-direct 模型。</span></div><div class="row"><select id="modelSelect" multiple size="5"></select><input id="accountName" placeholder="账户名，例如 ChatGPT OAuth"><input id="providerKey" type="password" placeholder="上游 API Key（ChatGPT 登录留空）" size="35"><input id="priority" type="number" value="0" placeholder="优先级"><input id="maxConcurrency" type="number" value="1" placeholder="并发数"><button onclick="createModelKey()">保存普通 API Key</button><button onclick="startCodexLogin()">开始 ChatGPT 授权</button></div><div id="oauthBox" class="notice">还没有开始 ChatGPT 授权。</div><table><thead><tr><th>ID</th><th>账户</th><th>类型</th><th>指纹</th><th>状态</th><th>优先级</th><th>并发</th><th>操作</th></tr></thead><tbody id="keys"></tbody></table></section>
<section class="admin-only"><h2>分配共享模型 Key</h2><div class="row"><label>分配给用户 <select id="userSelect"><option value="">请选择用户</option></select></label><label>选择模型 Key <select id="keySelect"><option value="">请选择模型 Key</option></select></label><label><input id="makeDefault" type="checkbox" checked>设为用户默认</label><button onclick="assignKey()">分配</button></div><table><thead><tr><th>用户</th><th>模型 Key</th><th>账户</th><th>默认</th><th>操作</th></tr></thead><tbody id="assignments"></tbody></table></section>
<section class="admin-only"><h2>生成用户访问 Key</h2><div class="row"><select id="gatewayUserSelect"></select><input id="gatewayKeyName" placeholder="Key 名称"><button onclick="createGatewayKey()">生成</button></div><div id="newKey" class="notice"></div><table><thead><tr><th>ID</th><th>用户</th><th>名称</th><th>前缀</th><th>状态</th><th>操作</th></tr></thead><tbody id="gatewayKeys"></tbody></table></section>
<div id="msg" class="notice"></div></main>
<script>
const $=id=>document.getElementById(id);let me=null,users=[],models=[],keys=[];
async function api(path,opt={}){let r=await fetch(path,{...opt,credentials:'same-origin',headers:{'Content-Type':'application/json',...(opt.headers||{})}});let d=await r.json().catch(()=>({error:r.statusText}));if(!r.ok)throw Error(d.error||'请求失败');return d}
function msg(s){$('msg').textContent=s||''} function ok(s){msg(s);alert(s)} function loginMsg(s){$('loginMsg').textContent=s||''}
async function login(){try{me=await api('/admin/api/login',{method:'POST',body:JSON.stringify({username:$('loginUser').value,password:$('loginPassword').value})});showApp();await loadAll()}catch(e){loginMsg(e.message)}}
async function logout(){await api('/admin/api/logout',{method:'POST'}).catch(()=>{});location.reload()}
function showApp(){$('login').classList.add('hidden');$('app').classList.remove('hidden');$('who').textContent=me.username+'（'+me.role+'）';document.querySelectorAll('.admin-only').forEach(x=>x.style.display=me.role==='admin'?'':'none')}
async function loadAll(){try{let data=await Promise.all([api('/admin/api/models'),api('/admin/api/providers')]);models=Array.isArray(data[0])?data[0]:[];renderProviders(data[1]);renderModels();if(me.role==='admin'){users=await api('/admin/api/users');users=Array.isArray(users)?users:[];renderUsers();renderUserSelects();await loadGatewayKeys();await loadAssignments()}await loadKeys();msg('已登录')}catch(e){msg(e.message)}}
function renderProviders(items){items=Array.isArray(items)?items:[];$('provider').innerHTML=items.map(p=>'<option value="'+p+'">'+p+'</option>').join('')}
function renderUsers(){users=Array.isArray(users)?users:[];$('users').innerHTML=users.map(u=>{let protectedAdmin=u.role==='admin'||u.username==='admin';let controls=protectedAdmin?'<span>管理员账户不可修改</span>':'<input id="pw-'+u.id+'" type="password" placeholder="新密码"><button onclick="resetPassword('+u.id+')">保存</button> <button onclick="deleteUser('+u.id+')">删除</button>';return '<tr><td>'+u.id+'</td><td>'+u.username+'</td><td>'+u.display_name+'</td><td>'+u.role+'</td><td>'+(u.shared_model_key_id||'')+'</td><td>'+controls+'</td></tr>'}).join('')}
function renderModels(){models=Array.isArray(models)?models:[];$('models').innerHTML=models.map(m=>'<tr><td>'+m.id+'</td><td>'+m.name+'</td><td>'+m.provider+'</td><td>'+m.upstream_model+'</td><td><button onclick="deleteModel('+m.id+')">删除</button></td></tr>').join('');$('modelSelect').innerHTML=models.map(m=>'<option value="'+m.id+'">'+m.id+' - '+m.name+' ['+m.provider+']</option>').join('');if(models.length)$('modelSelect').options[0].selected=true}
function selectedModelIDs(){return Array.from($('modelSelect').selectedOptions).map(x=>Number(x.value)).filter(x=>x>0)}
async function loadKeys(){let ids=selectedModelIDs();if(!ids.length){$('keys').innerHTML='';$('keySelect').innerHTML='<option value="">请选择模型 Key</option>';return}keys=await api('/admin/api/model-keys?model_ids='+ids.join(','));keys=Array.isArray(keys)?keys:[];$('keys').innerHTML=keys.map(k=>'<tr><td>'+k.id+'</td><td>'+k.account_name+'</td><td>'+k.auth_type+'</td><td><code>'+k.api_key_fingerprint.slice(0,16)+'</code></td><td>'+k.status+'</td><td>'+k.priority+'</td><td>'+k.max_concurrency+'</td><td><button onclick="deleteModelKey('+k.id+')">删除</button></td></tr>').join('');$('keySelect').innerHTML='<option value="">请选择模型 Key</option>'+keys.map(k=>'<option value="'+k.id+'">'+k.id+' - '+k.account_name+'</option>').join('')}
function renderUserSelects(){let userOptions='<option value="">请选择用户</option>'+users.map(u=>'<option value="'+u.id+'">'+u.id+' - '+u.username+'</option>').join('');$('userSelect').innerHTML=userOptions;$('gatewayUserSelect').innerHTML=userOptions}
async function loadAssignments(){let items=await api('/admin/api/assignments');items=Array.isArray(items)?items:[];$('assignments').innerHTML=items.map(a=>'<tr><td>'+a.user_id+' - '+a.username+'</td><td>'+a.model_key_id+'</td><td>'+a.account_name+'</td><td>'+(a.is_default?'是':'否')+'</td><td><button onclick="deleteAssignment('+a.user_id+','+a.model_key_id+')">删除</button></td></tr>').join('')}
async function createUser(){try{await api('/admin/api/users',{method:'POST',body:JSON.stringify({username:$('username').value,display_name:$('displayName').value,role:$('role').value,password:$('userPassword').value})});ok('用户创建成功');await loadAll()}catch(e){msg(e.message)}}
async function resetPassword(id){try{await api('/admin/api/users/password',{method:'POST',body:JSON.stringify({user_id:id,password:$('pw-'+id).value})});ok('密码已更新')}catch(e){msg(e.message)}}
async function deleteUser(id){if(confirm('确定删除这个用户？')){try{await api('/admin/api/users/delete',{method:'POST',body:JSON.stringify({user_id:id})});ok('用户已删除');await loadAll()}catch(e){msg(e.message)}}}
async function deleteModel(id){if(confirm('删除模型会解除其模型 Key 关联，确定继续？')){try{await api('/admin/api/models/delete',{method:'POST',body:JSON.stringify({model_id:id})});ok('模型已删除');await loadAll()}catch(e){msg(e.message)}}}
async function deleteModelKey(id){if(confirm('确定删除这个模型 Key？')){try{await api('/admin/api/model-keys/delete',{method:'POST',body:JSON.stringify({model_key_id:id})});ok('模型 Key 已删除');await loadKeys()}catch(e){msg(e.message)}}}
async function createModel(){try{await api('/admin/api/models',{method:'POST',body:JSON.stringify({name:$('modelName').value,provider:$('provider').value,upstream_model:$('upstreamModel').value})});ok('模型创建成功');await loadAll()}catch(e){msg(e.message)}}
async function createModelKey(){try{let ids=selectedModelIDs();if(!ids.length){msg('请至少选择一个模型');return}await api('/admin/api/model-keys',{method:'POST',body:JSON.stringify({model_ids:ids,account_name:$('accountName').value,api_key:$('providerKey').value,priority:+$('priority').value,max_concurrency:+$('maxConcurrency').value})});$('providerKey').value='';ok('模型 Key 保存成功');await loadKeys()}catch(e){msg(e.message)}}
async function startCodexLogin(){try{let ids=selectedModelIDs();if(!ids.length){msg('第一步：请先选择一个 openai-direct 模型');return}let selected=models.filter(m=>ids.includes(Number(m.id)));let wrong=selected.filter(m=>m.provider!=='openai-direct');if(wrong.length){msg('ChatGPT 授权只能绑定 openai-direct 模型；请取消选择：'+wrong.map(m=>m.name).join('、'));return}let account=$('accountName').value.trim()||'ChatGPT OAuth';$('accountName').value=account;let d=await api('/admin/api/codex/start',{method:'POST',body:JSON.stringify({model_ids:ids,account_name:account})});let popup=window.open(d.authorization_url,'_blank','noopener');$('oauthBox').innerHTML=(popup?'已打开 ChatGPT 授权页。':'浏览器拦截了新窗口，请')+'完成授权后回到这里等待绑定；'+(popup?'':'<a target="_blank" href="'+d.authorization_url+'">点击此处打开授权页</a>');let attempts=Math.ceil(d.expires_in/(d.interval||2));let n=0;let timer=setInterval(async()=>{if(++n>attempts){clearInterval(timer);$('oauthBox').textContent='授权已过期，请重新开始';return}try{let result=await api('/admin/api/codex/poll',{method:'POST',body:JSON.stringify({flow_token:d.flow_token})});if(result.status==='complete'){clearInterval(timer);$('oauthBox').textContent='绑定成功！现在可以把这个模型 Key 分配给用户。';alert('ChatGPT 授权成功');await loadKeys()}}catch(e){clearInterval(timer);$('oauthBox').textContent='授权失败：'+e.message}},(d.interval||2)*1000)}catch(e){$('oauthBox').textContent='授权启动失败：'+e.message}}
async function assignKey(){let userID=Number($('userSelect').value),keyID=Number($('keySelect').value);if(!userID){msg('请先选择要分配给的用户');return}if(!keyID){msg('请先选择要分配的模型 Key');return}try{await api('/admin/api/assignments',{method:'POST',body:JSON.stringify({user_id:userID,model_key_id:keyID,default:$('makeDefault').checked})});ok('共享模型 Key 分配成功');await loadAll()}catch(e){msg(e.message)}}
async function deleteAssignment(userID,keyID){if(confirm('确定取消这个用户的共享模型 Key？')){try{await api('/admin/api/assignments/delete',{method:'POST',body:JSON.stringify({user_id:userID,model_key_id:keyID})});ok('共享模型 Key 已取消');await loadAll()}catch(e){msg(e.message)}}}
async function createGatewayKey(){let userID=Number($('gatewayUserSelect').value);if(!userID){msg('请先选择访问 Key 所属用户');return}try{let d=await api('/admin/api/gateway-keys',{method:'POST',body:JSON.stringify({user_id:userID,name:$('gatewayKeyName').value})});$('newKey').textContent='新 Key（只显示一次）： '+d.key;ok('用户访问 Key 生成成功，请立即复制')}catch(e){msg(e.message)}}
async function loadGatewayKeys(){let items=await api('/admin/api/gateway-keys');items=Array.isArray(items)?items:[];$('gatewayKeys').innerHTML=items.map(k=>'<tr><td>'+k.id+'</td><td>'+k.username+'</td><td>'+k.name+'</td><td><code>'+k.key_prefix+'</code></td><td>'+k.status+'</td><td><button onclick="deleteGatewayKey('+k.id+')">删除</button></td></tr>').join('')}
async function deleteGatewayKey(id){if(confirm('删除后这个用户访问 Key 将立即失效，确定继续？')){try{await api('/admin/api/gateway-keys/delete',{method:'POST',body:JSON.stringify({key_id:id})});ok('用户访问 Key 已删除');await loadGatewayKeys()}catch(e){msg(e.message)}}}
$('modelSelect').addEventListener('change',loadKeys)
</script></html>`
