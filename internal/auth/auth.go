package auth

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/MoeclubM/metafusion-storage/internal/config"
	"github.com/MoeclubM/metafusion-storage/internal/upstream"
)

// Principal 是验签后的调用者身份。只信令牌里的字段：
// Groups 是组码、Permissions 是按组展开后的权限码集合，均由账号服务随令牌与 /api/auth/me 下发——字段名与签发侧逐字一致
// （metafusion-auth/internal/store/{token.go,store.go}）。
// 具体能编辑、能下载什么由本服务按权限码判断，见 permission.go。
type Principal struct {
	ID          string   `json:"id"`
	Username    string   `json:"username"`
	Groups      []string `json:"groups,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	// Scope/ClientID 是第三方 OAuth 授权的绑定标记（与签发侧对齐）：站内会话签发恒清零
	// 这两项；用途判定见 Verify（只认 token_use）。
	// 不进 JSON 输出、不暴露给调用方。
	Scope    string `json:"-"`
	ClientID string `json:"-"`
	// IsThirdParty 标记身份来自第三方 OAuth 授权（token_use=oauth/id_token）。
	// 治理类权限默认拒绝此类令牌（见 permission.go 的 Can），不进 JSON 输出。
	IsThirdParty bool `json:"-"`
	// FromPAT 区分 PAT 与 JWT，供审计记录凭据类型，不参与权限判定。
	FromPAT bool `json:"-"`
	// TokenVersion 来自 JWT tv；验签后与账号服务 credentials-status 核对。
	TokenVersion int64 `json:"-"`
}

// authPolicy 是账号服务出站调用的策略，会话兜底与 PAT 内省**共用同一份参数**：
// 两者指向同一个上游、同一套降级语义，参数分叉会让"同一个账号服务故障"在两个调用点上有两种表现。
// 超时分层与重试都收在这里，调用方不再各自传一个 http.Client 超时。
func authPolicy(name string) upstream.Policy {
	p := upstream.DefaultPolicy(name)
	p.Attempts = 2
	p.AttemptTimeout = 1500 * time.Millisecond
	p.Budget = 4 * time.Second
	p.BaseBackoff = 100 * time.Millisecond
	p.MaxBackoff = 400 * time.Millisecond
	p.Jitter = 0.5
	p.BreakerThreshold = 5
	p.BreakerOpenFor = 10 * time.Second
	return p
}

// Verifier 只做一件事：把请求换算成身份。
// 公钥来源优先取静态配置，其次按 JWKS 地址拉取并缓存（未知 kid 会触发一次强制刷新）。
type Verifier struct {
	issuer   string
	audience string
	jwksURL  string
	static   *rsa.PublicKey
	// client 只服务 JWKS 拉取，刻意**不**套 upstream 的重试与熔断：公钥拉取自带 10 分钟缓存、
	// 未知 kid 强刷与"刷新失败回落缓存公钥"的降级顺序，加一层重试/熔断会改变验签的降级路径。
	client *http.Client
	// pat 是个人访问令牌的内省器（见 pat.go）。为 nil（未配置 AUTH_URL）时，
	// 带 mfp_ 前缀的请求一律 503 auth_unavailable——身份只能问账号服务。
	pat *PATIntrospector
	// creds 核对会话/OAuth JWT 的 token_version 与封禁（见 credentials.go）。
	creds *CredentialsChecker

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	// refreshMu 只用来串行化"出站刷新"：flight 非空表示已有一次刷新在飞行中，
	// 等待者复用它的结果；flightErr 是最近一次飞行的结果。
	refreshMu sync.Mutex
	flight    chan struct{}
	flightErr error
}

type claims struct {
	Username    string   `json:"preferred_username"`
	Groups      []string `json:"groups,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	// Scope/ClientID/TokenUse 与签发侧对齐（auth Sign 恒写 token_use=session 并清零
	// scope/client_id；SignOAuth 写 token_use=oauth，SignForAudience 写 token_use=id_token）：
	// 用途判定只认 token_use（见 Verify 与 isThirdPartyUse），audience 收口
	// （Verify 的 WithAudience）已拒掉 aud 指向客户端的令牌，这里再拦“aud 仍是平台
	// 但带 OAuth 标记”的那一种。
	Scope    string `json:"scope"`
	ClientID string `json:"client_id"`
	TokenUse string `json:"token_use"`
	// TokenVersion 与签发侧 Claims.tv 对齐。
	TokenVersion int64 `json:"tv"`
	jwt.RegisteredClaims
}

// 令牌用途取值，与账号服务 store.TokenUse 同源；未知取值在 Verify 直接拒收。
const (
	TokenUseSession = "session"
	TokenUseOAuth   = "oauth"
	TokenUseIDToken = "id_token"
)

// isThirdPartyUse 报告该用途是否为第三方（oauth 访问令牌或发给当事客户端的 id_token）。
func isThirdPartyUse(use string) bool {
	return use == TokenUseOAuth || use == TokenUseIDToken
}

// validTokenUse 判定载荷里的用途声明是否合法：未知取值直接拒收，防止将来新增用途的
// 令牌被当成已知用途放行（与签发侧 validTokenUse 同口径）。
func validTokenUse(use string) bool {
	switch use {
	case TokenUseSession, TokenUseOAuth, TokenUseIDToken:
		return true
	default:
		return false
	}
}

const keyCacheTTL = 10 * time.Minute

func New(cfg config.Config) (*Verifier, error) {
	v := &Verifier{
		issuer:   cfg.JWTIssuer,
		audience: cfg.JWTAudience,
		jwksURL:  cfg.JWKSURL,
		client:   &http.Client{Timeout: 5 * time.Second},
		keys:     map[string]*rsa.PublicKey{},
	}
	if cfg.JWTPublicKeyPEM != "" {
		key, err := parsePublicKey(cfg.JWTPublicKeyPEM)
		if err != nil {
			return nil, err
		}
		v.static = key
	}
	return v, nil
}

// SetPAT 注入 PAT 内省器；nil 表示不接 PAT（带 mfp_ 的请求回 503 auth_unavailable）。
func (v *Verifier) SetPAT(p *PATIntrospector) { v.pat = p }

// SetCredentialsChecker 注入凭据世代核对器；未注入时跳过 tv/ban 核对（仅 JWKS）。
func (v *Verifier) SetCredentialsChecker(c *CredentialsChecker) { v.creds = c }

func parsePublicKey(raw string) (*rsa.PublicKey, error) {
	text := strings.TrimSpace(raw)
	if !strings.Contains(text, "BEGIN") {
		if decoded, err := base64.StdEncoding.DecodeString(text); err == nil {
			text = string(decoded)
		}
	}
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("AUTH_JWT_PUBLIC_KEY must be PEM or base64 PEM")
	}
	if block.Type != "PUBLIC KEY" && block.Type != "RSA PUBLIC KEY" {
		return nil, errors.New("AUTH_JWT_PUBLIC_KEY must be an RSA public key")
	}
	key, err := parseRSAPublic(block.Bytes)
	if err != nil {
		return nil, errors.New("AUTH_JWT_PUBLIC_KEY must be an RSA public key")
	}
	return key, nil
}

func parseRSAPublic(der []byte) (*rsa.PublicKey, error) {
	if parsed, err := x509.ParsePKIXPublicKey(der); err == nil {
		if key, ok := parsed.(*rsa.PublicKey); ok {
			return key, nil
		}
	}
	if key, err := x509.ParsePKCS1PublicKey(der); err == nil {
		return key, nil
	}
	return nil, errors.New("not an RSA public key")
}

// Verify 校验令牌并返回身份；失败一律返回错误，调用方自己决定 401 与否。
func (v *Verifier) Verify(token string) (*Principal, error) {
	if token == "" {
		return nil, errors.New("missing token")
	}
	parsed, err := jwt.ParseWithClaims(token, &claims{}, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.publicKey(kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(v.issuer), jwt.WithAudience(v.audience))
	if err != nil {
		return nil, err
	}
	c, ok := parsed.Claims.(*claims)
	if !ok || !parsed.Valid {
		return nil, errors.New("invalid token")
	}
	if c.Subject == "" {
		return nil, errors.New("token without subject")
	}
	clientID := c.ClientID
	// 用途隔离（与签发侧及互动 internal/auth/auth.go:325 同一契约）：会话 JWT 恒带
	// token_use=session 按第一方放行；只有 oauth/id_token 是第三方；未知用途直接拒收。
	use := strings.TrimSpace(c.TokenUse)
	if !validTokenUse(use) {
		return nil, errors.New("bad token_use")
	}
	thirdParty := isThirdPartyUse(use)
	return &Principal{ID: c.Subject, Username: c.Username, Groups: c.Groups, Permissions: c.Permissions, Scope: strings.TrimSpace(c.Scope), ClientID: strings.TrimSpace(clientID), IsThirdParty: thirdParty, TokenVersion: c.TokenVersion}, nil
}

// publicKey 按 kid 取验签公钥：命中未过期缓存直接返回；TTL 过期或 kid 未知时刷新一次
// （轮换期间签名密钥可能刚换过）。三条约束与目录/互动侧同构（2026-09-19 第二轮架构报告 #22）：
//
//  1. 出站请求不持缓存锁——JWKS 慢不会让本服务把请求堵在验签上；
//  2. 同一时刻只允许一次出站刷新（并发未知 kid 共享结果），出站请求数有界；
//  3. 刷新失败不直接判死：缓存里仍有该 kid 的公钥（TTL 过期也算——公钥本身没有有效期，
//     TTL 只是"多久去确认一次轮换"）就继续用它验签并记告警。账号服务抖动超过 TTL 不该让
//     本服务把所有已签发的有效令牌判成 401。
//
// 缓存里没有该 kid 时仍然 fail closed。
func (v *Verifier) publicKey(kid string) (*rsa.PublicKey, error) {
	if v.static != nil {
		return v.static, nil
	}
	if key, ok := v.freshKey(kid); ok {
		return key, nil
	}
	if err := v.refreshJWKS(kid); err != nil {
		if key, ok := v.cachedKey(kid); ok {
			slog.Warn("storage: JWKS 刷新失败，回落到缓存公钥（可能错过一次轮换确认）", "kid", kid, "err", err.Error())
			return key, nil
		}
		return nil, err
	}
	if key, ok := v.cachedKey(kid); ok {
		return key, nil
	}
	return nil, errors.New("unknown signing key")
}

// freshKey 返回缓存里命中且未过期的公钥。
func (v *Verifier) freshKey(kid string) (*rsa.PublicKey, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if time.Since(v.fetchedAt) >= keyCacheTTL {
		return nil, false
	}
	return lookupKey(v.keys, kid)
}

// cachedKey 返回缓存里该 kid 的公钥，不看是否过期：刷新失败时的回落来源。
func (v *Verifier) cachedKey(kid string) (*rsa.PublicKey, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return lookupKey(v.keys, kid)
}

// lookupKey 按 kid 取键；只有一个密钥且令牌头没带 kid 时也命中（账号服务总是带 kid，这条只是容错）。
func lookupKey(keys map[string]*rsa.PublicKey, kid string) (*rsa.PublicKey, bool) {
	if key, ok := keys[kid]; ok {
		return key, true
	}
	if kid == "" && len(keys) == 1 {
		for _, key := range keys {
			return key, true
		}
	}
	return nil, false
}

// refreshJWKS 拉一次 JWKS。出站请求不持 mu；refreshMu + flight 让同一时刻只有一次出站请求，
// 等待者复用同一次结果，不再各自打一次网络。未知 kid 强刷、命中缓存不刷的行为保持不变
// （因此这里不再需要旧实现那次"再取一次"的额外出站）。
func (v *Verifier) refreshJWKS(kid string) error {
	v.refreshMu.Lock()
	if ch := v.flight; ch != nil {
		v.refreshMu.Unlock()
		<-ch
		v.refreshMu.Lock()
		err := v.flightErr
		v.refreshMu.Unlock()
		return err
	}
	// 拿到刷新权时缓存可能刚被刷好：命中就直接复用，不必再打一次网络。
	if key, ok := v.freshKey(kid); ok && key != nil {
		v.refreshMu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	v.flight = ch
	v.refreshMu.Unlock()

	err := v.fetch()

	v.refreshMu.Lock()
	v.flightErr, v.flight = err, nil
	v.refreshMu.Unlock()
	close(ch)
	return err
}

// fetch 拉取 JWKS 并整体替换缓存。非 200、解析失败或没有可用 RSA 公钥都算失败，
// 此时旧缓存保持不动（publicKey 会按回落规则继续使用它）。出站期间不持 mu。
func (v *Verifier) fetch() error {
	if v.jwksURL == "" {
		return errors.New("no jwks url")
	}
	req, err := http.NewRequest(http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("jwks unavailable")
	}
	var doc struct {
		Keys []struct {
			KID string `json:"kid"`
			KTY string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if !strings.EqualFold(k.KTY, "RSA") {
			continue
		}
		key, err := rsaFromJWK(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.KID] = key
	}
	if len(keys) == 0 {
		return errors.New("jwks has no usable key")
	}
	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = time.Now()
	v.mu.Unlock()
	return nil
}

func rsaFromJWK(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, err
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	if e == 0 {
		return nil, errors.New("invalid exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}

// Middleware 解析身份但不拦截：带令牌且有效时写入上下文，供需要时读取。
// PAT 路径例外：无效 PAT 401 invalid_token、账号服务不可达 503 auth_unavailable，
// 这两种情况必须在这里结束请求（放行成匿名会让调用方拿到语义错误的 401
// authentication_required，把"依赖故障"误报成"凭据错"）。
func (v *Verifier) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		p, status := v.resolve(c)
		if rejectPAT(c, status) {
			return
		}
		if p != nil {
			c.Set(principalKey, p)
		}
		c.Next()
	}
}

// Required 要求已登录：无有效身份直接 401（PAT 的失败语义见 Middleware）。
func (v *Verifier) Required() gin.HandlerFunc {
	return func(c *gin.Context) {
		p, status := v.resolve(c)
		if rejectPAT(c, status) {
			return
		}
		if p != nil {
			c.Set(principalKey, p)
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication_required"})
	}
}

// resolve 把请求换算成身份与终局状态。
//
// PAT（Authorization: Bearer mfp_...）**只走内省**，且不回落到 cookie：浏览器里可能同时存在
// 另一个用户的 mf_session，回落到它会把"机器身份"悄悄变成"浏览器登录身份"。
// JWT 与不透明会话令牌的路径完全不变：验签失败仍按匿名继续（fail closed），
// 由各端点的 Required/require 决定 401——账号服务挂在 /api/auth/me 兜底上时，
// 这里也不会把普通浏览器的匿名读请求变成 503。
func (v *Verifier) resolve(c *gin.Context) (*Principal, patStatus) {
	bearer, cookie := credentials(c.Request)
	if IsPAT(bearer) {
		ident, err := v.introspectPAT(c.Request.Context(), bearer)
		switch {
		case err != nil:
			return nil, patUnavailable
		case ident == nil:
			return nil, patInvalid
		}
		return ident, patOK
	}
	credential := bearer
	if credential == "" {
		credential = cookie
	}
	if p, err := v.Verify(credential); err == nil {
		if _, ok, unavailable := v.checkCredentials(c.Request.Context(), p); unavailable {
			return nil, patUnavailable
		} else if !ok {
			return nil, patOK // 世代不匹配或已封禁：按匿名，Required 会 401
		}
		return p, patOK
	}
	return nil, patOK
}

// checkCredentials 在 JWKS 验签后核对账号库世代。未配置核对器时放行（ok=true）。
func (v *Verifier) checkCredentials(ctx context.Context, p *Principal) (st CredentialsStatus, ok bool, unavailable bool) {
	if v == nil || v.creds == nil || !v.creds.Enabled() || p == nil {
		return CredentialsStatus{}, true, false
	}
	st, err := v.creds.Lookup(ctx, p.ID)
	if err != nil {
		return CredentialsStatus{}, false, true
	}
	if !st.Match(p.TokenVersion) {
		return st, false, false
	}
	return st, true, false
}

// introspectPAT 是 PAT 的内省入口：内省器未注入（未配置 AUTH_URL）时按不可用处理。
// 身份只能问账号服务，本服务不查 auth 库，也不在本地缓存明文。
func (v *Verifier) introspectPAT(ctx context.Context, token string) (*Principal, error) {
	if v == nil || v.pat == nil {
		return nil, errPATUnavailable
	}
	ident, err := v.pat.Introspect(ctx, token)
	if err != nil {
		return nil, err
	}
	if ident == nil {
		return nil, nil
	}
	return ident.principal(), nil
}

func credentials(r *http.Request) (string, string) {
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	bearer := ""
	if len(authz) > 7 && strings.EqualFold(authz[:7], "bearer ") {
		bearer = strings.TrimSpace(authz[7:])
	}
	cookie := ""
	if ck, err := r.Cookie("mf_session"); err == nil && ck.Value != "" {
		cookie = ck.Value
	}
	return bearer, cookie
}

const principalKey = "storage_principal"

// Current 返回当前请求者，未登录为 nil。
func Current(c *gin.Context) *Principal {
	if v, ok := c.Get(principalKey); ok {
		if p, ok := v.(*Principal); ok {
			return p
		}
	}
	return nil
}
