package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MoeclubM/metafusion-storage/internal/upstream"
)

// ErrNotVisible 报告目录服务**明确回答**"该实体对当前请求者不可见或不存在"。
//
// 它必须与"上游不可用"分开：旧实现两种情况的返回值都是 ok=false，于是目录服务一抖就被调用方
// 折成"没有这个实体"——绑定在它上面的文件会从实体文件列表里静默消失，故障对用户和运维都不可观测。
// 调用方按错误分工：errors.Is(err, ErrNotVisible) → 404，其它 error → 503 上游机器码。
var ErrNotVisible = errors.New("entity not visible")

// visiblePolicy 是"问一次实体可见性"的出站策略。可见性判定在请求路径上（每次下载、每个列表都问），
// 因此单次尝试与总预算都收得紧：两次尝试吸收瞬时抖动，持续失败交给熔断挡住，
// 不给目录服务"抖一下就等于所有文件都不存在"的机会。
func visiblePolicy() upstream.Policy {
	p := upstream.DefaultPolicy("catalog")
	p.Attempts = 2
	p.AttemptTimeout = 2 * time.Second
	p.Budget = 5 * time.Second
	p.BaseBackoff = 100 * time.Millisecond
	p.MaxBackoff = 500 * time.Millisecond
	p.Jitter = 0.5
	p.BreakerThreshold = 5
	p.BreakerOpenFor = 10 * time.Second
	return p
}

// Client 是存储服务对元数据目录的唯一出口：只读实体可见性。
// 存储侧不复制目录数据、不直连目录库，可见性规则只有 catalog 一处实现；
// 用户会话解析由账号服务负责；目录身份解析统一走 identity 协议。
type Client struct {
	base string
	up   *upstream.Client
}

func New(base string) *Client {
	return &Client{
		base: strings.TrimRight(strings.TrimSpace(base), "/"),
		up:   upstream.New(visiblePolicy()),
	}
}

// Upstream 暴露出站执行器给深探针：/ready?deep=1 必须复用请求路径上的同一个熔断器，
// 另建一个实例只会得到一个永远闭合的假状态。
func (c *Client) Upstream() *upstream.Client { return c.up }

// Visible 通过统一身份契约询问可见性，合并身份和别名均由目录服务解析。
// ErrNotVisible 仅表示目录明确回答 not_found，其余错误由调用方返回依赖故障。
func (c *Client) Visible(ctx context.Context, entityID, bearer, cookie string) (string, error) {
	identity, err := c.Identity(ctx, entityID, bearer, cookie)
	if err != nil {
		return "", err
	}
	return identity.Entity.Kind, nil
}

// IdentityResolution 是目录侧统一身份解析契约（X01）的本地投影：canonical_id 为存活身份，
// aliases 为历史别名全集（GET /api/catalog/entities/{id}/identity，见目录
// lifecycle.go 的 IdentityResolution）：正向链上经过的旧 ID + 存活身份的反向遍历全集，
// 去重。complete=true 才能按全集展开读聚合（见 AliasSet）。
type IdentityResolution struct {
	CanonicalID string   `json:"canonical_id"`
	Aliases     []string `json:"aliases"`
	Complete    bool     `json:"complete"`
	Entity      struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	} `json:"entity"`
}

// Identity 问目录服务拿存活身份与 kind（X01 身份契约的只读投影，不改写任何引用）。
//
// identity 是必需的目录协议；不再回退到缺少反向别名全集的旧 /resolve 组合。
// error 非 nil 时不能当成“不可见”：只有 errors.Is(err, ErrNotVisible) 是目录服务的明确结论，
// 其余错误表示问不到上游，调用方必须回 503 而不是空列表。
func (c *Client) Identity(ctx context.Context, entityID, bearer, cookie string) (IdentityResolution, error) {
	var zero IdentityResolution
	if entityID == "" {
		return zero, ErrNotVisible
	}
	if c.base == "" {
		return zero, &upstream.Error{Upstream: "catalog", Reason: upstream.ReasonNotConfigured, Cause: errors.New("CATALOG_URL is required")}
	}
	header := http.Header{}
	c.decorate(header, bearer, cookie)
	resp, err := c.up.Do(ctx, upstream.Request{
		Method: http.MethodGet,
		URL:    c.base + "/api/catalog/entities/" + entityID + "/identity",
		Header: header,
	})
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		var v IdentityResolution
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			return zero, fmt.Errorf("catalog identity %s: 响应无法解析: %w", entityID, err)
		}
		if v.CanonicalID == "" || v.Entity.ID != v.CanonicalID || v.Entity.Kind == "" {
			return zero, fmt.Errorf("catalog identity %s: 响应缺 canonical", entityID)
		}
		if !v.Complete {
			return zero, &upstream.Error{Upstream: "catalog", Reason: upstream.ReasonBadResponse, Cause: errors.New("identity payload without complete alias set")}
		}
		return v, nil
	case resp.StatusCode == http.StatusNotFound:
		var response struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&response) == nil && response.Error == "not_found" {
			return zero, ErrNotVisible
		}
		return zero, fmt.Errorf("catalog identity %s: identity endpoint missing or unexpected 404 response", entityID)
	default:
		return zero, fmt.Errorf("catalog identity %s: status %d", entityID, resp.StatusCode)
	}
}

// AliasSet 组装一次读取要覆盖的 ID 集合：{canonical + 请求 ID + 目录返回的别名全集}
// 去重（与互动 internal/catalog/client.go:AliasSet 同一口径）。目录 identity 契约返回
// 正向链与反向全集后，调用方把 aliases 原样传入即完成 X01 全量聚合。
func AliasSet(canonical string, requested ...string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, id := range append(append([]string{}, requested...), canonical) {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func (c *Client) decorate(header http.Header, bearer, cookie string) {
	if bearer != "" {
		header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		header.Add("Cookie", (&http.Cookie{Name: "mf_session", Value: cookie}).String())
	}
}
