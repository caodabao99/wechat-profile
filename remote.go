package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RemoteClient 桌面端远程调用 bot 的 HTTP 客户端
type RemoteClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewRemoteClient 创建远程客户端
func NewRemoteClient(baseURL, token string) *RemoteClient {
	return &RemoteClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http: &http.Client{
			// 必须大于服务端的 LLM 重试窗口（60s×2 + 2s ≈ 122s）和
			// ingestAnalyzeTimeout（100s），否则会在服务端还在正常重试时掐断请求，
			// 用户看到「超时」但服务端其实已经把画像写好了。
			Timeout: 180 * time.Second,
		},
	}
}

// do 执行 HTTP 请求并解析 JSON 响应
func (c *RemoteClient) do(method, path string, body interface{}, result interface{}) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.baseURL+path, reqBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respData, &errResp) == nil && errResp.Error != "" {
			return fmt.Errorf("%s", errResp.Error)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respData))
	}

	if result != nil {
		return json.Unmarshal(respData, result)
	}
	return nil
}

// ---- 联系人查询 ----

// ContactJSON API 返回的联系人结构
type ContactJSON struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Remark         string   `json:"remark"`
	ProfileJSON    string   `json:"profileJson,omitempty"`
	ProfileSummary string   `json:"profileSummary"`
	OtherMsgCount  int      `json:"otherMsgCount"`
	LastUpdated    string   `json:"lastUpdated"`
	CreatedAt      string   `json:"createdAt"`
	MergedInto     int64    `json:"mergedInto"`
	MergeCount     int      `json:"mergeCount"`
	Aliases        []string `json:"aliases,omitempty"`
}

// GetContacts 获取联系人列表
func (c *RemoteClient) GetContacts(includeMerged bool) ([]ContactJSON, error) {
	var out []ContactJSON
	path := "/api/contacts"
	if includeMerged {
		path += "?includeMerged=1"
	}
	err := c.do("GET", path, nil, &out)
	return out, err
}

// GetContact 获取单个联系人详情
func (c *RemoteClient) GetContact(id int64) (*ContactJSON, error) {
	var out ContactJSON
	err := c.do("GET", fmt.Sprintf("/api/contacts/%d", id), nil, &out)
	return &out, err
}

// MessageJSON API 返回的消息结构
type MessageJSON struct {
	Sender  string `json:"sender"`
	Content string `json:"content"`
	MsgTime string `json:"msgTime"`
}

// GetMessages 获取联系人消息（分页）
func (c *RemoteClient) GetMessages(id int64, offset, limit int) ([]MessageJSON, error) {
	var out []MessageJSON
	path := fmt.Sprintf("/api/contacts/%d/messages?offset=%d&limit=%d", id, offset, limit)
	err := c.do("GET", path, nil, &out)
	return out, err
}

// ContactStatsJSON API 返回的统计结构
type ContactStatsJSON struct {
	Total     int64  `json:"Total"`
	Mine      int64  `json:"Mine"`
	Other     int64  `json:"Other"`
	FirstTime string `json:"FirstTime"`
	LastTime  string `json:"LastTime"`
}

// GetStats 获取联系人统计
func (c *RemoteClient) GetStats(id int64) (*ContactStatsJSON, error) {
	var out ContactStatsJSON
	err := c.do("GET", fmt.Sprintf("/api/contacts/%d/stats", id), nil, &out)
	return &out, err
}

// ProfileHistoryJSON API 返回的画像历史结构
type ProfileHistoryJSON struct {
	ID            int64  `json:"ID"`
	ContactID     int64  `json:"ContactID"`
	ProfileJSON   string `json:"ProfileJSON"`
	ChangeSummary string `json:"ChangeSummary"`
	CreatedAt     string `json:"CreatedAt"`
}

// GetHistory 获取画像历史
func (c *RemoteClient) GetHistory(id int64, limit int) ([]ProfileHistoryJSON, error) {
	var out []ProfileHistoryJSON
	path := fmt.Sprintf("/api/contacts/%d/history?limit=%d", id, limit)
	err := c.do("GET", path, nil, &out)
	return out, err
}

// ---- 联系人写操作 ----

// ResolveContact 解析联系人名称到 ID
func (c *RemoteClient) ResolveContact(name string) (int64, bool, error) {
	var out struct {
		ID       int64 `json:"id"`
		ViaAlias bool  `json:"viaAlias"`
	}
	err := c.do("POST", "/api/contacts/resolve", map[string]string{"name": name}, &out)
	return out.ID, out.ViaAlias, err
}

// CreateContact 创建联系人
func (c *RemoteClient) CreateContact(name string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := c.do("POST", "/api/contacts", map[string]string{"name": name}, &out)
	return out.ID, err
}

// SetRemark 设置备注
func (c *RemoteClient) SetRemark(id int64, remark string) error {
	return c.do("POST", fmt.Sprintf("/api/contacts/%d/remark", id),
		map[string]string{"remark": remark}, nil)
}

// SetName 设置名称
func (c *RemoteClient) SetName(id int64, name string) error {
	return c.do("POST", fmt.Sprintf("/api/contacts/%d/name", id),
		map[string]string{"name": name}, nil)
}

// DeleteContact 删除联系人
func (c *RemoteClient) DeleteContact(id int64) error {
	return c.do("DELETE", fmt.Sprintf("/api/contacts/%d", id), nil, nil)
}

// ---- LLM 操作 ----

func (c *RemoteClient) EditProfile(id int64, profile Profile, base string) error {
	return c.do("PUT", fmt.Sprintf("/api/contacts/%d/profile", id), map[string]interface{}{"profile": profile, "baseProfileJson": base}, nil)
}

// SupplementProfile 补充画像
func (c *RemoteClient) SupplementProfile(id int64, note string) error {
	return c.do("POST", fmt.Sprintf("/api/contacts/%d/supplement", id),
		map[string]string{"note": note}, nil)
}

// RegenerateProfile 重新生成画像
func (c *RemoteClient) RegenerateProfile(id int64) (string, error) {
	var out struct {
		OK      string `json:"ok"`
		Summary string `json:"summary"`
	}
	err := c.do("POST", fmt.Sprintf("/api/contacts/%d/regenerate", id), nil, &out)
	return out.Summary, err
}

// AnalyzeIntent 意图分析
func (c *RemoteClient) AnalyzeIntent(id int64, message string) (map[string]interface{}, error) {
	var out map[string]interface{}
	err := c.do("POST", fmt.Sprintf("/api/contacts/%d/analyze", id),
		map[string]string{"message": message}, &out)
	return out, err
}

func (c *RemoteClient) RewriteReply(id int64, text, style string) (string, error) {
	var out struct {
		Reply string `json:"reply"`
	}
	err := c.do("POST", fmt.Sprintf("/api/contacts/%d/rewrite", id), map[string]string{"text": text, "style": style}, &out)
	return out.Reply, err
}
func (c *RemoteClient) ReviewDraft(id int64, text string) (*DraftReview, error) {
	var out DraftReview
	err := c.do("POST", fmt.Sprintf("/api/contacts/%d/review-draft", id), map[string]string{"text": text}, &out)
	return &out, err
}
func (c *RemoteClient) ProfileChanges(id int64) (ProfileChanges, error) {
	var out ProfileChanges
	err := c.do("GET", fmt.Sprintf("/api/contacts/%d/profile-changes", id), nil, &out)
	return out, err
}

// ---- Personal Relationship OS 2.0：关系状态机 / Memory Replay / Decision Engine ----
// 这三项是服务端能力（statemachine/decision/replay 只存在于 bot 仓），
// 桌面端仅在远程模式下经此调用；本地模式下无对应实现，分发层会给出明确提示。

// RelationshipStateJSON 对应服务端 statemachine.RelationshipStateView 的 JSON（字段名严格一致）。
type RelationshipStateJSON struct {
	ContactID    int64  `json:"contactId"`
	Name         string `json:"name"`
	BaseState    string `json:"baseState"`
	DynamicState string `json:"dynamicState"`
	Intimacy     int    `json:"intimacy"`
	TrendState   string `json:"trendState"`
	Alert        string `json:"alert"`
	Health       int    `json:"health"`
	Reason       string `json:"reason"`
	ChangedAt    string `json:"changedAt"`
	ComputedAt   string `json:"computedAt"`
}

// ContactState GET /api/contacts/{id}/state：关系状态机单联系人快照。
// 服务端在尚无快照时返回 404，do() 会转成 error，调用方据此提示。
func (c *RemoteClient) ContactState(id int64) (*RelationshipStateJSON, error) {
	var out struct {
		State RelationshipStateJSON `json:"state"`
	}
	if err := c.do("GET", fmt.Sprintf("/api/contacts/%d/state", id), nil, &out); err != nil {
		return nil, err
	}
	return &out.State, nil
}

// ContactReplay GET /api/contacts/{id}/replay：Memory Replay「重新认识 TA」，
// 服务端已用 RenderReplayText 渲染成纯文本，桌面端直接展示 rendered 字段。
func (c *RemoteClient) ContactReplay(id int64) (string, error) {
	var out struct {
		OK       bool   `json:"ok"`
		Rendered string `json:"rendered"`
	}
	if err := c.do("GET", fmt.Sprintf("/api/contacts/%d/replay", id), nil, &out); err != nil {
		return "", err
	}
	return out.Rendered, nil
}

// DecisionCandidateJSON 对应服务端 decision.DecisionCandidate 的 JSON（snake_case tag 严格一致）。
type DecisionCandidateJSON struct {
	ContactID    int64    `json:"contact_id"`
	Name         string   `json:"name"`
	State        string   `json:"state"`
	BaseState    string   `json:"base_state"`
	DynamicState string   `json:"dynamic_state"`
	Priority     int      `json:"priority"`
	Risk         int      `json:"risk"`
	Opportunity  int      `json:"opportunity"`
	Intimacy     int      `json:"intimacy"`
	Goal         string   `json:"goal,omitempty"`
	Followup     string   `json:"followup,omitempty"`
	Topic        string   `json:"topic,omitempty"`
	Action       string   `json:"action,omitempty"`
	BestTime     string   `json:"best_time,omitempty"`
	Source       string   `json:"source"`
	Confidence   string   `json:"confidence"`
	ReasonCodes  []string `json:"reason_codes"`
	WhyNow       []string `json:"why_now"`
}

// TodayDecisions GET /api/decision/today?top=N：今天最值得投入的关系行动 Top N（确定性排序，非 LLM）。
func (c *RemoteClient) TodayDecisions(top int) ([]DecisionCandidateJSON, error) {
	var out struct {
		OK        bool                    `json:"ok"`
		Count     int                     `json:"count"`
		Decisions []DecisionCandidateJSON `json:"decisions"`
	}
	if err := c.do("GET", fmt.Sprintf("/api/decision/today?top=%d", top), nil, &out); err != nil {
		return nil, err
	}
	return out.Decisions, nil
}

// ---- 合并 ----

// MergeResultJSON 合并结果
//
// 字段名与服务端 MergeResult 的 Go 字段名一一对应（服务端未加 json tag，
// 直接按字段名序列化），改名会静默拿到零值。
type MergeResultJSON struct {
	MovedMessages int   `json:"MovedMessages"`
	MovedHistory  int   `json:"MovedHistory"`
	MergeLogID    int64 `json:"MergeLogID"`
	// ProfileCopied 为 true 表示服务端把 source 的画像拷给了 target。
	// 只用于界面文案；真正的回滚由服务端 UndoMerge 依据 merge_log 完成。
	ProfileCopied bool `json:"ProfileCopied"`
}

// MergeContacts 合并联系人
func (c *RemoteClient) MergeContacts(sourceID, targetID int64, useSourceName, regenerate bool) (*MergeResultJSON, error) {
	var out MergeResultJSON
	err := c.do("POST", "/api/merge", map[string]interface{}{
		"sourceId":      sourceID,
		"targetId":      targetID,
		"useSourceName": useSourceName,
		"regenerate":    regenerate,
	}, &out)
	return &out, err
}

// UndoMerge 撤销合并
func (c *RemoteClient) UndoMerge(logID int64) error {
	return c.do("POST", "/api/merge/undo", map[string]int64{"logId": logID}, nil)
}

// MergeLogJSON 合并日志
type MergeLogJSON struct {
	ID         int64  `json:"ID"`
	SourceID   int64  `json:"SourceID"`
	TargetID   int64  `json:"TargetID"`
	SourceName string `json:"SourceName"`
	TargetName string `json:"TargetName"`
	CreatedAt  string `json:"CreatedAt"`
	UndoneAt   string `json:"UndoneAt"`
}

// GetMergeLogs 获取合并日志
func (c *RemoteClient) GetMergeLogs(limit int) ([]MergeLogJSON, error) {
	var out []MergeLogJSON
	path := fmt.Sprintf("/api/merge/logs?limit=%d", limit)
	err := c.do("GET", path, nil, &out)
	return out, err
}

// GetMergeLogsForTarget 获取指定目标联系人的合并日志
func (c *RemoteClient) GetMergeLogsForTarget(targetID int64) ([]MergeLogJSON, error) {
	var out []MergeLogJSON
	path := fmt.Sprintf("/api/merge/logs?targetId=%d", targetID)
	err := c.do("GET", path, nil, &out)
	return out, err
}

// ---- 核心：聊天记录识别 ----

// IngestResult 识别结果
type IngestResult struct {
	ContactID        int64                  `json:"contactId"`
	ContactName      string                 `json:"contactName"`
	DisplayName      string                 `json:"displayName"`
	ParsedCount      int                    `json:"parsedCount"`
	NewCount         int                    `json:"newCount"`
	OtherMsgCount    int                    `json:"otherMsgCount"`
	ViaAlias         bool                   `json:"viaAlias"`
	ProfileTriggered bool                   `json:"profileTriggered"`
	HasProfile       bool                   `json:"hasProfile"`
	ColdStartCount   int                    `json:"coldStartCount"`
	Intent           map[string]interface{} `json:"intent,omitempty"`
	IntentError      string                 `json:"intentError,omitempty"`
}

// Ingest 提交聊天记录进行识别
func (c *RemoteClient) Ingest(text string, analyze bool) (*IngestResult, error) {
	var out IngestResult
	err := c.do("POST", "/api/ingest", map[string]interface{}{
		"text":    text,
		"analyze": analyze,
	}, &out)
	return &out, err
}

// ---- 状态 ----

// Status 服务状态
func (c *RemoteClient) Status() (map[string]interface{}, error) {
	var out map[string]interface{}
	err := c.do("GET", "/api/status", nil, &out)
	return out, err
}

// Ping 测试连接
func (c *RemoteClient) Ping() error {
	return c.do("GET", "/api/status", nil, nil)
}

// ---- 备份 / 恢复（zip 较大，用 30 分钟超时的独立 client） ----

// BackupRestoreJSON 服务端恢复结果
type BackupRestoreJSON = BackupSummary

func (c *RemoteClient) backupHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Minute}
}

// DownloadBackup 从服务端下载备份 zip，返回文件内容与建议文件名。
//
// password 非空时用 POST 带口令，服务端会把 zip 内的密钥文件加密后再打包；
// 留空则走 GET，得到明文备份（与旧版服务端行为一致）。
// 口令放请求体而不是查询串，避免落进服务端访问日志和浏览器历史。
func (c *RemoteClient) DownloadBackup(password string) ([]byte, string, error) {
	var req *http.Request
	var err error
	if password != "" {
		body, _ := json.Marshal(map[string]string{"password": password})
		req, err = http.NewRequest(http.MethodPost, c.baseURL+"/api/backup/export", bytes.NewReader(body))
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, err = http.NewRequest(http.MethodGet, c.baseURL+"/api/backup/export", nil)
		if err != nil {
			return nil, "", err
		}
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.backupHTTPClient().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, "", fmt.Errorf("导出备份失败 (HTTP %d): %s", resp.StatusCode, string(b))
	}
	// 上限 512MB，防止异常响应打爆内存
	data, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		return nil, "", err
	}
	name := "wechat-profile-backup.zip"
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if m := strings.Index(cd, "filename*=UTF-8''"); m >= 0 {
			raw := cd[m+len("filename*=UTF-8''"):]
			if i := strings.IndexAny(raw, ";,"); i >= 0 {
				raw = raw[:i]
			}
			if dec, err := url.QueryUnescape(raw); err == nil && dec != "" {
				name = dec
			}
		}
	}
	return data, name, nil
}

// UploadBackup 把本地备份 zip 上传到服务端恢复。
// password 用于解密 zip 内被加密的密钥文件；备份未加密时传空串。
func (c *RemoteClient) UploadBackup(zipPath, password string) (*BackupRestoreJSON, error) {
	f, err := os.Open(zipPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	// 密码字段放在文件之前：文件可能上百 MB，会被落到临时文件，
	// 而小字段留在内存里，服务端 ParseMultipartForm 后能直接取到。
	if password != "" {
		if err := mw.WriteField("password", password); err != nil {
			return nil, err
		}
	}
	part, err := mw.CreateFormFile("file", filepath.Base(zipPath))
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return nil, err
	}
	mw.Close()

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/backup/import", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.backupHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respData, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respData, &errResp) == nil && errResp.Error != "" {
			return nil, fmt.Errorf("%s", errResp.Error)
		}
		return nil, fmt.Errorf("恢复失败 (HTTP %d): %s", resp.StatusCode, string(respData))
	}
	var out BackupRestoreJSON
	if err := json.Unmarshal(respData, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- 工具函数 ----

// ParseURL 解析并标准化 API 地址
func ParseAPIURL(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("地址不能为空")
	}
	if !strings.HasPrefix(input, "http://") && !strings.HasPrefix(input, "https://") {
		input = "http://" + input
	}
	u, err := url.Parse(input)
	if err != nil {
		return "", err
	}
	if u.Port() == "" {
		u.Host += ":17965"
	}
	return u.String(), nil
}
