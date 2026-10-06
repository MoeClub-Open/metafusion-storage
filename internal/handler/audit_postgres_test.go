package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-storage/internal/audit"
	"github.com/MoeclubM/metafusion-storage/internal/auth"
	"github.com/MoeclubM/metafusion-storage/internal/catalog"
	"github.com/MoeclubM/metafusion-storage/internal/config"
	"github.com/MoeclubM/metafusion-storage/internal/objects"
	"github.com/MoeclubM/metafusion-storage/internal/store"
	"github.com/MoeclubM/metafusion-storage/internal/testutil"
)

// auditHarness 是"真库 + 真路由"的审计用例载体：真令牌验签、真落库、真写审计表。
// 与 uploadHarness 分开是因为这里要断言审计行本身，而 uploadHarness 不接记录器。
type auditHarness struct {
	t           *testing.T
	r           *gin.Engine
	db          *sql.DB
	rec         *audit.Recorder
	key         *rsa.PrivateKey
	kid         string
	requestIDs  []string
	newReqCount int
}

func newAuditHarness(t *testing.T) *auditHarness {
	t.Helper()
	return newAuditHarnessWith(t, false)
}

// newAuditHarnessWith 允许注入一个"存在但不可用"的 PAT 内省器（baseURL 为空）：
// 形态非法的 PAT 在本地就被判否（401 invalid_token），形态合法的 PAT 因账号服务未配置而 503
// auth_unavailable——两条路径都不打外部网络，也不需要账号服务桩。
func newAuditHarnessWith(t *testing.T, withPAT bool) *auditHarness {
	t.Helper()
	db := testutil.Database(t)
	ctx := context.Background()
	st, err := store.Open(ctx, testutil.DSN(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err = st.Init(ctx); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	// 用例之间互不影响：先删子表再删母表（绑定对 assets 有外键）。
	if _, err = db.ExecContext(ctx, "DELETE FROM storage.bindings"); err != nil {
		t.Fatalf("clean bindings: %v", err)
	}
	if _, err = db.ExecContext(ctx, "DELETE FROM storage.assets"); err != nil {
		t.Fatalf("clean assets: %v", err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	jwks, kid := jwksServer(t, key)

	// 本地对象模式：不需要 S3 兼容端就能走完 initiate → stream → complete。
	cfg := config.Config{
		Root:         t.TempDir(),
		S3Bucket:     "metafusion-test",
		PresignTTL:   5 * time.Minute,
		MaxPartCount: 10000,
	}
	objs, err := objects.New(ctx, cfg)
	if err != nil {
		t.Fatalf("objects.New: %v", err)
	}
	if !objs.Local() {
		t.Fatal("用例依赖本地对象模式（未配置 S3 端点）")
	}

	// 目录服务只被问可见性：所有实体都可见。
	cat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/catalog/entities/"), "/identity")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"canonical_id": id, "aliases": []string{}, "complete": true,
			"entity": map[string]any{"id": id, "kind": "track"},
		})
	}))
	t.Cleanup(cat.Close)

	h := &auditHarness{t: t, db: db, key: key, kid: kid}
	h.rec = audit.NewRecorder(db, audit.ServiceName)
	// 收尾先排空队列（断言要看到全部行），再删掉本次用例写下的审计行。
	t.Cleanup(func() {
		h.rec.Close()
		for _, id := range h.requestIDs {
			if _, err := db.ExecContext(context.Background(), "DELETE FROM audit.audit_log WHERE request_id = $1", id); err != nil {
				t.Logf("清理审计行 %s 失败: %v", id, err)
			}
		}
	})

	verifier := newVerifier(t, jwks.URL)
	if withPAT {
		verifier.SetPAT(auth.NewPATIntrospector(""))
	}

	gin.SetMode(gin.TestMode)
	h.r = gin.New()
	New(st, objs, catalog.New(cat.URL), verifier, cfg).UseAudit(h.rec).Register(h.r)
	return h
}

// requestID 生成一个本次用例唯一、可读的 request_id（同时也是清理键）。
func (h *auditHarness) requestID(prefix string) string {
	h.newReqCount++
	return prefix + "-" + uuid.NewString()
}

// do 发一次 JSON 请求；requestID 为空表示不带 X-Request-Id（响应头会回写生成值）。
func (h *auditHarness) do(token, method, path, body, requestID string) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return h.send(req, token, requestID)
}

// put 发一次原始字节请求（流式上传收的就是文件本身，不能当 JSON 解析）。
func (h *auditHarness) put(token, path string, body []byte, requestID string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/octet-stream")
	return h.send(req, token, requestID)
}

func (h *auditHarness) send(req *http.Request, token, requestID string) *httptest.ResponseRecorder {
	h.t.Helper()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if requestID != "" {
		req.Header.Set("X-Request-Id", requestID)
		h.requestIDs = append(h.requestIDs, requestID)
	}
	w := httptest.NewRecorder()
	h.r.ServeHTTP(w, req)
	if requestID == "" {
		// 中间件生成的 id 只出现在响应头里：登记下来供清理使用。
		if generated := w.Header().Get("X-Request-Id"); generated != "" {
			h.requestIDs = append(h.requestIDs, generated)
		}
	}
	return w
}

// auditRow 是一行审计行（actor_user_id 用空串表示 NULL）。
type auditRow struct {
	id             string
	occurredAt     time.Time
	service        string
	action         string
	actorUserID    string
	actorUsername  string
	credentialType string
	actorIP        string
	actorUserAgent string
	targetType     string
	targetID       string
	changes        string
	result         string
	errorCode      string
	requestMethod  string
	route          string
	httpStatus     int
	requestID      string
}

const selectAuditRow = "SELECT id::text, occurred_at, service, action, coalesce(actor_user_id::text, ''), actor_username, credential_type, actor_ip, actor_user_agent, target_type, target_id, changes::text, result, error_code, request_method, route, http_status, request_id FROM audit.audit_log WHERE request_id = $1 ORDER BY occurred_at, id::text"

const selectAuditRowText = "SELECT concat_ws(' | ', id::text, occurred_at::text, service, action, coalesce(actor_user_id::text, ''), actor_username, credential_type, actor_ip, actor_user_agent, target_type, target_id, changes::text, result, error_code, request_method, route, http_status::text, request_id) FROM audit.audit_log WHERE request_id = $1"

// rows 取某个 request_id 的审计行（应恰好一条；多于一条说明同一动作被重复留痕）。
func (h *auditHarness) rows(requestID string) []auditRow {
	h.t.Helper()
	rs, err := h.db.QueryContext(context.Background(), selectAuditRow, requestID)
	if err != nil {
		h.t.Fatalf("查审计行: %v", err)
	}
	defer rs.Close()
	out := []auditRow{}
	for rs.Next() {
		var row auditRow
		if err := rs.Scan(&row.id, &row.occurredAt, &row.service, &row.action, &row.actorUserID,
			&row.actorUsername, &row.credentialType, &row.actorIP, &row.actorUserAgent,
			&row.targetType, &row.targetID, &row.changes, &row.result, &row.errorCode,
			&row.requestMethod, &row.route, &row.httpStatus, &row.requestID); err != nil {
			h.t.Fatalf("扫审计行: %v", err)
		}
		out = append(out, row)
	}
	if err := rs.Err(); err != nil {
		h.t.Fatalf("遍历审计行: %v", err)
	}
	return out
}

// one 断言某个 request_id 恰好一行、动作码正确，并返回该行（契约 §6.2 的"恰好一行"）。
func (h *auditHarness) one(requestID, action string) auditRow {
	h.t.Helper()
	rows := h.rows(requestID)
	if len(rows) != 1 {
		h.t.Fatalf("request_id=%s 应恰好一条审计行，实际 %d 条：%+v", requestID, len(rows), rows)
	}
	if rows[0].action != action {
		h.t.Fatalf("request_id=%s 的动作码 = %q，期望 %q", requestID, rows[0].action, action)
	}
	return rows[0]
}

// rowText 把整行拼成文本：敏感值判据（契约 §6.2）断言在这上面，而不是只看 changes。
func (h *auditHarness) rowText(requestID string) string {
	h.t.Helper()
	texts := []string{}
	rs, err := h.db.QueryContext(context.Background(), selectAuditRowText, requestID)
	if err != nil {
		h.t.Fatalf("查整行文本: %v", err)
	}
	defer rs.Close()
	for rs.Next() {
		var text string
		if err := rs.Scan(&text); err != nil {
			h.t.Fatalf("扫整行文本: %v", err)
		}
		texts = append(texts, text)
	}
	if err := rs.Err(); err != nil {
		h.t.Fatalf("遍历整行文本: %v", err)
	}
	if len(texts) != 1 {
		h.t.Fatalf("request_id=%s 的整行文本应有 1 条，实际 %d", requestID, len(texts))
	}
	return texts[0]
}

// changesOf 解析审计行的 changes（jsonb 文本形式）。
func (h *auditHarness) changesOf(row auditRow) map[string]any {
	h.t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal([]byte(row.changes), &out); err != nil {
		h.t.Fatalf("解析 changes 失败: %v（%s）", err, row.changes)
	}
	return out
}

// assertRequestIDEcho 断言 X-Request-Id 透传/回写。
func assertRequestIDEcho(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := w.Header().Get("X-Request-Id"); got != want {
		t.Fatalf("响应头 X-Request-Id = %q，期望 %q", got, want)
	}
}

// 敏感值判据（契约 §6.2）：口令/令牌/邀请码与 PAT 前缀，以及完整邮箱。
var (
	sensitivePattern = regexp.MustCompile("(?i)password|token|secret|mfp_|mf_pat_|sk_")
	fullEmailPattern = regexp.MustCompile("[A-Za-z0-9._%+\\-]+@[A-Za-z0-9](?:[A-Za-z0-9\\-]*[A-Za-z0-9])?(?:\\.[A-Za-z0-9](?:[A-Za-z0-9\\-]*[A-Za-z0-9])?)+")
)

// assertRowClean 断言整行文本里没有敏感值、也没有完整邮箱；changes 还必须在 8KB 以内。
func (h *auditHarness) assertRowClean(row auditRow) {
	h.t.Helper()
	text := h.rowText(row.requestID)
	if m := sensitivePattern.FindAllString(text, -1); len(m) != 0 {
		h.t.Fatalf("审计行 %s 命中敏感值 %v：\n%s", row.action, m, text)
	}
	if m := fullEmailPattern.FindAllString(text, -1); len(m) != 0 {
		h.t.Fatalf("审计行 %s 里出现完整邮箱 %v：\n%s", row.action, m, text)
	}
	if len(row.changes) > audit.MaxChangesBytes {
		h.t.Fatalf("changes 序列化后 %d 字节，超过上限 %d", len(row.changes), audit.MaxChangesBytes)
	}
}

// TestAuditLogAgainstPostgres 是契约 §6.2 的真库用例：每个被审计动作恰好一行、X-Request-Id 透传、
// 整行零敏感命中，并且流式上传的请求体内容绝不进审计行。
func TestAuditLogAgainstPostgres(t *testing.T) {
	h := newAuditHarness(t)
	owner := signTokenWith(t, h.key, h.kid, "user", nil, []string{auth.PermissionAssetUpload})

	// 请求体里塞口令与 PAT 前缀：它们只应存在于对象内容里，绝不能出现在审计行。
	body := append(onePixelPNGBytes(t), []byte("\npassword=never-audit-me mfp_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG")...)
	sha := sha256HexOf(body)
	const entityID = "01a0a88d-cd52-745a-9266-b62aea6f8296"

	// 1) initiate：file_name 里带一个完整邮箱，用来验证值级遮罩真的落到了库里。
	initiateID := h.requestID("initiate")
	payload, _ := json.Marshal(map[string]any{
		"file_name":   "invoice-jane.doe@example.com.bin",
		"file_size":   len(body),
		"sha256_hash": sha,
		"mime_type":   "image/png",
		"part_count":  1,
	})
	w := h.do(owner, http.MethodPost, "/api/storage/upload/initiate", string(payload), initiateID)
	if w.Code != http.StatusOK {
		t.Fatalf("initiate 返回 %d：%s", w.Code, w.Body.String())
	}
	assertRequestIDEcho(t, w, initiateID)
	var initiated struct {
		AssetID string `json:"asset_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &initiated); err != nil || initiated.AssetID == "" {
		t.Fatalf("解析 initiate 响应失败: %v（%s）", err, w.Body.String())
	}

	// 2) stream：服务端接收路径，落定资产。
	streamID := h.requestID("stream")
	w = h.put(owner, "/api/storage/upload/stream/"+initiated.AssetID, body, streamID)
	if w.Code != http.StatusOK {
		t.Fatalf("stream 返回 %d：%s", w.Code, w.Body.String())
	}
	assertRequestIDEcho(t, w, streamID)

	// 3) complete：本地模式下资产已 complete，走幂等确认分支。
	completeID := h.requestID("complete")
	completePayload, _ := json.Marshal(map[string]any{"asset_id": initiated.AssetID})
	w = h.do(owner, http.MethodPost, "/api/storage/upload/complete", string(completePayload), completeID)
	if w.Code != http.StatusOK {
		t.Fatalf("complete 返回 %d：%s", w.Code, w.Body.String())
	}

	// 4) bind：绑定这份文件到目录实体。
	bindID := h.requestID("bind")
	bindPayload, _ := json.Marshal(map[string]any{
		"asset_id":         initiated.AssetID,
		"target_entity_id": entityID,
		"binding_role":     "cover_image",
	})
	w = h.do(owner, http.MethodPost, "/api/storage/bind", string(bindPayload), bindID)
	if w.Code != http.StatusOK {
		t.Fatalf("bind 返回 %d：%s", w.Code, w.Body.String())
	}
	var bound struct {
		Binding struct {
			ID string `json:"id"`
		} `json:"binding"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &bound); err != nil || bound.Binding.ID == "" {
		t.Fatalf("解析 bind 响应失败: %v（%s）", err, w.Body.String())
	}

	// 5) unbind：解绑（本服务唯一的删除动作）。
	unbindID := h.requestID("unbind")
	w = h.do(owner, http.MethodDelete, "/api/storage/bindings/"+bound.Binding.ID, "", unbindID)
	if w.Code != http.StatusOK {
		t.Fatalf("unbind 返回 %d：%s", w.Code, w.Body.String())
	}

	// 排空队列后再断言：记录器是异步的，Close 之后所有行都已在库里。
	h.rec.Close()

	// 每个动作恰好一行。
	initRow := h.one(initiateID, "asset.upload_initiated")
	streamRow := h.one(streamID, "asset.upload_streamed")
	completeRow := h.one(completeID, "asset.upload_completed")
	bindRow := h.one(bindID, "binding.created")
	unbindRow := h.one(unbindID, "binding.removed")

	// 行形状：service/actor/方法/路由模板/状态码/时间。
	for _, row := range []auditRow{initRow, streamRow, completeRow, bindRow, unbindRow} {
		if row.service != audit.ServiceName {
			t.Fatalf("service = %q，期望 %q", row.service, audit.ServiceName)
		}
		if row.actorUserID != "11111111-1111-1111-1111-111111111111" || row.actorUsername != "kana" {
			t.Fatalf("操作者快照不符: %+v", row)
		}
		if row.credentialType != audit.CredentialSession {
			t.Fatalf("JWT 会话应为 session，实际 %q", row.credentialType)
		}
		if row.actorIP == "" {
			t.Fatal("actor_ip 不该为空")
		}
		if row.result != "success" || row.errorCode != "" || row.httpStatus != http.StatusOK {
			t.Fatalf("成功行应为 success/空错误码/200: %+v", row)
		}
		if row.requestMethod == "" || !strings.HasPrefix(row.route, "/api/storage/") {
			t.Fatalf("方法/路由模板不符: %+v", row)
		}
		if time.Since(row.occurredAt) > 5*time.Minute {
			t.Fatalf("occurred_at 过旧: %v", row.occurredAt)
		}
		h.assertRowClean(row)
	}

	// route 存模板而不是原始路径（聚合价值就在于此）。
	if streamRow.route != "/api/storage/upload/stream/:assetId" {
		t.Fatalf("route 应为模板，实际 %q", streamRow.route)
	}
	if unbindRow.route != "/api/storage/bindings/:id" {
		t.Fatalf("route 应为模板，实际 %q", unbindRow.route)
	}

	// 被动对象与摘要。
	if initRow.targetType != "asset" || initRow.targetID != initiated.AssetID {
		t.Fatalf("initiate 行的 target 不符: %+v", initRow)
	}
	initChanges := h.changesOf(initRow)
	if initChanges["sha256"] != sha || initChanges["created"] != true {
		t.Fatalf("initiate 行摘要不符: %#v", initChanges)
	}
	// 值里的邮箱被遮罩（完整邮箱已经在 assertRowClean 里断言零命中）。
	if name, _ := initChanges["file_name"].(string); !strings.Contains(name, "i***@example.com.bin") {
		t.Fatalf("file_name 里的邮箱应被遮罩（保留首字母与域名），实际 %q", name)
	}
	// 摘要里的键名不能撞上脱敏的子串黑名单（含 hash 的键会被整条替换成 [redacted]）。
	if initChanges["verified"] != false {
		t.Fatalf("未完成资产的 verified 应为 false，实际 %#v", initChanges["verified"])
	}
	streamChanges := h.changesOf(streamRow)
	if streamChanges["size_bytes"] != float64(len(body)) {
		t.Fatalf("stream 行应记收到的大小: %#v", streamChanges)
	}
	transition, ok := streamChanges["status"].(map[string]any)
	if !ok || transition["from"] != "pending" || transition["to"] != "complete" {
		t.Fatalf("stream 行应记状态迁移: %#v", streamChanges)
	}
	if streamChanges["verified"] != true {
		t.Fatalf("落定后 verified 应为 true（键名撞黑名单才会被替换成 [redacted]）: %#v", streamChanges)
	}
	// 请求体是文件本身：它的任何片段都不该进审计行（含口令/令牌样式的内容）。
	if text := h.rowText(streamID); strings.Contains(text, "never-audit-me") {
		t.Fatalf("审计行里出现了请求体片段：\n%s", text)
	}
	if bindRow.targetType != "binding" || bindRow.targetID != bound.Binding.ID {
		t.Fatalf("bind 行的 target 不符: %+v", bindRow)
	}
	bindChanges := h.changesOf(bindRow)
	if bindChanges["asset_id"] != initiated.AssetID || bindChanges["binding_role"] != "cover_image" || bindChanges["target_entity_id"] != entityID {
		t.Fatalf("bind 行摘要不符: %#v", bindChanges)
	}
	unbindChanges := h.changesOf(unbindRow)
	if unbindChanges["removed"] != true || unbindChanges["asset_id"] != initiated.AssetID {
		t.Fatalf("unbind 行摘要不符: %#v", unbindChanges)
	}
	completeChanges := h.changesOf(completeRow)
	if completeChanges["already_complete"] != true {
		t.Fatalf("complete 行应记幂等确认: %#v", completeChanges)
	}
}

// TestAuditLogAgainstPostgresFailurePaths：失败也留痕（result=failure + error_code），
// 且 GET 与豁免路由（verify-hash）零留痕。
func TestAuditLogAgainstPostgresFailurePaths(t *testing.T) {
	h := newAuditHarness(t)
	uploader := signTokenWith(t, h.key, h.kid, "user", nil, []string{auth.PermissionAssetUpload})
	// 只持审核码：上传类写路由应 403（缺 storage.asset.upload）。
	moderator := signTokenWith(t, h.key, h.kid, "user", nil, []string{auth.PermissionAssetModerate})

	// 1) 403：权限闸门拒掉的写尝试同样要留痕。
	forbiddenID := h.requestID("forbidden")
	payload, _ := json.Marshal(map[string]any{
		"file_name": "x.bin", "file_size": 1, "sha256_hash": strings.Repeat("a", 64), "part_count": 1,
	})
	w := h.do(moderator, http.MethodPost, "/api/storage/upload/initiate", string(payload), forbiddenID)
	if w.Code != http.StatusForbidden {
		t.Fatalf("缺上传权限应 403，实际 %d：%s", w.Code, w.Body.String())
	}
	assertRequestIDEcho(t, w, forbiddenID)

	// 2) 400：载荷不合法（缺 target_entity_id）。
	badPayloadID := h.requestID("bad-payload")
	badPayload, _ := json.Marshal(map[string]any{"asset_id": uuid.NewString()})
	w = h.do(uploader, http.MethodPost, "/api/storage/bind", string(badPayload), badPayloadID)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法载荷应 400，实际 %d：%s", w.Code, w.Body.String())
	}

	// 3) 404：解绑一个不存在的绑定。
	notFoundID := h.requestID("not-found")
	w = h.do(uploader, http.MethodDelete, "/api/storage/bindings/"+uuid.NewString(), "", notFoundID)
	if w.Code != http.StatusNotFound {
		t.Fatalf("解绑不存在的绑定应 404，实际 %d：%s", w.Code, w.Body.String())
	}

	// 4) 豁免：verify-hash 是读语义的 POST，不写审计。
	exemptID := h.requestID("exempt")
	exemptPayload, _ := json.Marshal(map[string]any{"sha256_hash": strings.Repeat("b", 64)})
	w = h.do("", http.MethodPost, "/api/storage/verify-hash", string(exemptPayload), exemptID)
	if w.Code != http.StatusOK {
		t.Fatalf("verify-hash 应 200，实际 %d：%s", w.Code, w.Body.String())
	}

	// 5) GET 不记。
	getID := h.requestID("get")
	w = h.do(uploader, http.MethodGet, "/api/storage/assets/"+uuid.NewString(), "", getID)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET 不存在的资产应 404，实际 %d：%s", w.Code, w.Body.String())
	}

	h.rec.Close()

	forbiddenRow := h.one(forbiddenID, "asset.upload_initiated")
	if forbiddenRow.result != "failure" || forbiddenRow.errorCode != "forbidden" || forbiddenRow.httpStatus != http.StatusForbidden {
		t.Fatalf("403 行应记 failure/forbidden/403: %+v", forbiddenRow)
	}
	badPayloadRow := h.one(badPayloadID, "binding.created")
	if badPayloadRow.result != "failure" || badPayloadRow.errorCode != "invalid_payload" || badPayloadRow.httpStatus != http.StatusBadRequest {
		t.Fatalf("400 行应记 failure/invalid_payload/400: %+v", badPayloadRow)
	}
	notFoundRow := h.one(notFoundID, "binding.removed")
	if notFoundRow.result != "failure" || notFoundRow.errorCode != "not_found" || notFoundRow.httpStatus != http.StatusNotFound {
		t.Fatalf("404 行应记 failure/not_found/404: %+v", notFoundRow)
	}
	for _, row := range []auditRow{forbiddenRow, badPayloadRow, notFoundRow} {
		h.assertRowClean(row)
	}
	if rows := h.rows(exemptID); len(rows) != 0 {
		t.Fatalf("豁免路由 verify-hash 不该留痕，实际 %d 行: %+v", len(rows), rows)
	}
	if rows := h.rows(getID); len(rows) != 0 {
		t.Fatalf("GET 不该留痕，实际 %d 行: %+v", len(rows), rows)
	}
}

// TestAuditLogAgainstPostgresGeneratesRequestID：不带 X-Request-Id 时中间件生成 uuid 并回写，
// 审计行的 request_id 与响应头一致（网关与应用日志靠这个关联同一次操作）。
func TestAuditLogAgainstPostgresGeneratesRequestID(t *testing.T) {
	h := newAuditHarness(t)
	uploader := signTokenWith(t, h.key, h.kid, "user", nil, []string{auth.PermissionAssetUpload})

	payload, _ := json.Marshal(map[string]any{"asset_id": uuid.NewString()})
	w := h.do(uploader, http.MethodPost, "/api/storage/bind", string(payload), "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法载荷应 400，实际 %d：%s", w.Code, w.Body.String())
	}
	generated := w.Header().Get("X-Request-Id")
	if _, err := uuid.Parse(generated); err != nil {
		t.Fatalf("缺省应生成 uuid 并回写响应头，实际 %q", generated)
	}

	h.rec.Close()
	row := h.one(generated, "binding.created")
	if row.requestID != generated {
		t.Fatalf("审计行的 request_id 应是回写的那个: %q vs %q", row.requestID, generated)
	}
}

// TestAuditLogAgainstPostgresAuthRejectedWrites：身份中间件（v.Required / v.Middleware）在审计中间件
// 之后运行，因此**被它拒掉的写请求也留痕**——否则"伪造 PAT 打写接口"这类请求在库里完全没有痕迹。
//
// 这些响应由身份中间件用 AbortWithStatusJSON 直接写出（没走 handler.fail），所以 error_code 按契约回落
// 成 http_<status>；actor 为空、credential_type=anonymous（身份没解析出来）；处理器没跑到，因此没有 target。
func TestAuditLogAgainstPostgresAuthRejectedWrites(t *testing.T) {
	h := newAuditHarnessWith(t, true)
	payload, _ := json.Marshal(map[string]any{
		"asset_id":         uuid.NewString(),
		"target_entity_id": uuid.NewString(),
		"binding_role":     "cover_image",
	})

	cases := []struct {
		name       string
		authHeader string // 空串表示完全不带 Authorization
		wantStatus int
		wantBody   string
	}{
		{"形态非法的 PAT", "Bearer mfp_short", http.StatusUnauthorized, auth.CodeInvalidToken},
		{"形态合法但账号服务不可达", "Bearer mfp_" + strings.Repeat("A", 43), http.StatusServiceUnavailable, auth.CodeAuthUnavailable},
		{"无效 JWT", "Bearer not.a.jwt", http.StatusUnauthorized, "authentication_required"},
		{"完全没有凭据", "", http.StatusUnauthorized, "authentication_required"},
	}

	requestIDs := map[string]string{}
	for _, tc := range cases {
		id := h.requestID("auth-reject")
		requestIDs[tc.name] = id
		req := httptest.NewRequest(http.MethodPost, "/api/storage/bind", strings.NewReader(string(payload)))
		req.Header.Set("Content-Type", "application/json")
		if tc.authHeader != "" {
			req.Header.Set("Authorization", tc.authHeader)
		}
		w := h.send(req, "", id)
		if w.Code != tc.wantStatus || !strings.Contains(w.Body.String(), tc.wantBody) {
			t.Fatalf("%s：期望 %d %s，实际 %d %s", tc.name, tc.wantStatus, tc.wantBody, w.Code, w.Body.String())
		}
		assertRequestIDEcho(t, w, id)
	}

	h.rec.Close()

	for _, tc := range cases {
		// 恰好一行：身份拒掉的请求不能被重复留痕。
		row := h.one(requestIDs[tc.name], "binding.created")
		if row.result != "failure" {
			t.Fatalf("%s：应记 failure，实际 %+v", tc.name, row)
		}
		wantCode := "http_" + strconv.Itoa(tc.wantStatus)
		if row.errorCode != wantCode || row.httpStatus != tc.wantStatus {
			t.Fatalf("%s：error_code 应回落 %s（响应就是它），实际 %q/%d", tc.name, wantCode, row.errorCode, row.httpStatus)
		}
		if row.credentialType != audit.CredentialAnonymous || row.actorUserID != "" || row.actorUsername != "" {
			t.Fatalf("%s：身份没解析出来，应按匿名记，实际 %+v", tc.name, row)
		}
		if row.route != "/api/storage/bind" || row.requestMethod != http.MethodPost {
			t.Fatalf("%s：路由模板/方法不符: %+v", tc.name, row)
		}
		if row.targetType != "" || row.targetID != "" {
			t.Fatalf("%s：处理器没跑到，不该有 target: %+v", tc.name, row)
		}
		h.assertRowClean(row)
	}
}

// TestAuditLogAgainstPostgresAuthRejectedWritesWithoutIntrospector 固定另一半口径：
// 完全没有注入 PAT 内省器时（AUTH_URL 未配置），internal/auth 对任何 `mfp_` 前缀都按
// "依赖不可用"返回，**形态非法的 PAT 也一样 503**（见 auth.go 的 introspectPAT 注释：身份只能问
// 账号服务，问不到就不是"凭据错"）。这是既有口径、不是本次改动引入的，但它同样必须留痕。
func TestAuditLogAgainstPostgresAuthRejectedWritesWithoutIntrospector(t *testing.T) {
	h := newAuditHarness(t)
	payload, _ := json.Marshal(map[string]any{
		"asset_id":         uuid.NewString(),
		"target_entity_id": uuid.NewString(),
		"binding_role":     "cover_image",
	})
	id := h.requestID("auth-reject-no-introspector")
	req := httptest.NewRequest(http.MethodPost, "/api/storage/bind", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer mfp_short")
	w := h.send(req, "", id)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), auth.CodeAuthUnavailable) {
		t.Fatalf("未配置内省器时任何 mfp_ 前缀都应按依赖不可用回 503，实际 %d %s", w.Code, w.Body.String())
	}

	h.rec.Close()
	row := h.one(id, "binding.created")
	if row.result != "failure" || row.errorCode != "http_503" || row.httpStatus != http.StatusServiceUnavailable {
		t.Fatalf("应记 failure/http_503/503，实际 %+v", row)
	}
	if row.credentialType != audit.CredentialAnonymous || row.actorUserID != "" {
		t.Fatalf("身份没解析出来，应按匿名记: %+v", row)
	}
	h.assertRowClean(row)
}
