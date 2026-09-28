// Package xffprobe runs a live capability test against the upstream anonymous
// trial channel to answer one question: does upstream honour a forged
// X-Forwarded-For value, and does it accept IPv6 literals as well as IPv4?
//
// Method (no generation is consumed unless quota must be observed):
//
//  1. Pick a random IPv4 key A and a random IPv6 key B.
//  2. Read /usage with key A and with key B. A fresh key reports remaining=2.
//  3. Submit one real generation under key A. If upstream keys its allowance on
//     the XFF value, a re-read of /usage under A must report remaining=1.
//  4. Repeat 3 for key B with an IPv6 literal.
//
// A family counts as supported when its own key decrements. Because each family
// uses an independent key, one family failing cannot mask the other.
package xffprobe

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yhw5231/H3Gateway/internal/config"
	"github.com/yhw5231/H3Gateway/internal/identity"
	"github.com/yhw5231/H3Gateway/internal/model"
	"github.com/yhw5231/H3Gateway/internal/upstream"
)

// Result is the outcome of a capability run.
type Result struct {
	RanAt        time.Time `json:"ran_at"`
	OK           bool      `json:"ok"`
	Upstream     string    `json:"upstream"`
	IPv4Accepted bool      `json:"ipv4_accepted"`
	IPv6Accepted bool      `json:"ipv6_accepted"`
	// DryRun marks a reachability-only run, where the accepted flags are
	// deliberately left false because nothing was submitted.
	DryRun  bool     `json:"dry_run,omitempty"`
	Message string   `json:"message"`
	Steps   []string `json:"steps,omitempty"`

	IPv4Key       string `json:"ipv4_key,omitempty"`
	IPv6Key       string `json:"ipv6_key,omitempty"`
	IPv4Remaining *int   `json:"ipv4_remaining,omitempty"`
	IPv6Remaining *int   `json:"ipv6_remaining,omitempty"`
}

// Record converts the result into the persisted model.
func (r *Result) Record() *model.XFFProbeRecord {
	return &model.XFFProbeRecord{
		RanAt:        r.RanAt,
		OK:           r.OK,
		Upstream:     r.Upstream,
		IPv4Accepted: r.IPv4Accepted,
		IPv6Accepted: r.IPv6Accepted,
		DryRun:       r.DryRun,
		Message:      r.Message,
		Steps:        append([]string(nil), r.Steps...),
	}
}

// Options controls how thorough the probe is.
type Options struct {
	// Image is the payload submitted for the generation step. A probe cannot
	// prove quota keying without consuming one generation per family.
	Image []byte
	// Filename names the uploaded part.
	Filename string
	// DryRun performs only the quota reads, which proves reachability but not
	// quota keying.
	DryRun bool
}

// Run performs the capability test.
func Run(ctx context.Context, up *upstream.Client, settings config.Settings, opts Options) *Result {
	res := &Result{
		RanAt:    time.Now(),
		Upstream: settings.UpstreamBase,
	}
	step := func(format string, args ...any) {
		res.Steps = append(res.Steps, fmt.Sprintf(format, args...))
	}

	// The probe deliberately talks to the upstream directly. A proxy supplies
	// the real egress address and would overwrite the forged header, so routing
	// the test through one would make it measure nothing.
	if len(settings.ProxyList) > 0 {
		step("注意：已配置 %d 个代理，但 XFF 能力测试仍直连上游（代理会覆盖伪造的 X-Forwarded-For）",
			len(settings.ProxyList))
	}

	// --- reachability ------------------------------------------------------
	if _, err := up.Quota(ctx, "", ""); err != nil {
		res.Message = "无法访问上游 /usage 接口：" + err.Error()
		step("GET %s -> %v", settings.UsageURL(), err)
		return res
	}
	step("GET %s 可达", settings.UsageURL())
	res.OK = true

	ipv4Key := identity.RandomIPv4(identity.PoolReserved)
	ipv6Key := identity.RandomIPv6(identity.PoolReserved, false)
	res.IPv4Key, res.IPv6Key = ipv4Key, ipv6Key

	before4, err4 := up.Quota(ctx, ipv4Key, "")
	if err4 != nil {
		step("IPv4 配额读取失败: %v", err4)
	} else {
		step("IPv4 键 %s 初始 remaining=%d", ipv4Key, before4.Remaining)
	}
	before6, err6 := up.Quota(ctx, ipv6Key, "")
	if err6 != nil {
		step("IPv6 配额读取失败: %v", err6)
	} else {
		step("IPv6 键 %s 初始 remaining=%d", ipv6Key, before6.Remaining)
	}

	if opts.DryRun {
		// Reachability only: nothing was submitted, so no claim about which
		// header value drives the allowance may be made.
		res.DryRun = true
		res.IPv4Accepted = false
		res.IPv6Accepted = false
		res.Message = "仅完成连通性检查（未消耗配额）。IPv4/IPv6 是否可作为独立配额键尚未验证，" +
			"请运行完整测试后再启用「随机 IPv6」。"
		return res
	}
	if len(opts.Image) == 0 {
		res.Message = "缺少测试图片，无法执行消耗配额的完整测试。"
		return res
	}

	// --- IPv4 quota keying -------------------------------------------------
	res.IPv4Accepted = probeFamily(ctx, up, settings, opts, ipv4Key, model.FamilyIPv4, before4, &res.IPv4Remaining, step)
	// --- IPv6 quota keying -------------------------------------------------
	res.IPv6Accepted = probeFamily(ctx, up, settings, opts, ipv6Key, model.FamilyIPv6, before6, &res.IPv6Remaining, step)

	switch {
	case res.IPv4Accepted && res.IPv6Accepted:
		res.Message = "随机公网 XFF 生效，且 IPv4 与 IPv6 都被上游作为独立配额键接受 —— 后台已可启用「随机 IPv6」。"
	case res.IPv4Accepted && !res.IPv6Accepted:
		res.Message = "随机公网 XFF（IPv4）生效，但 IPv6 未被识别为独立配额键；请保持 IPv4 模式。"
	case !res.IPv4Accepted && res.IPv6Accepted:
		res.Message = "IPv6 配额键生效，但 IPv4 未通过验证（可能该 IPv4 键已被其他使用者耗尽）。"
	default:
		res.Message = "未能确认 XFF 配额键行为：上游可能已改为按真实出口 IP 或账号计数。"
	}
	return res
}

// probeFamily submits one generation under key and reports whether the same key
// lost a unit of quota, which is what proves the header drives the allowance.
func probeFamily(ctx context.Context, up *upstream.Client, settings config.Settings, opts Options,
	key, family string, before *upstream.Usage, remainingOut **int, step func(string, ...any)) bool {

	ident := identity.Mint(family, identity.PoolReserved, false)
	ident.ForgedIP = key

	if _, err := up.Submit(ctx, upstream.SubmitOptions{
		Image:    opts.Image,
		Filename: opts.Filename,
		Prompt:   "A calm, natural camera move.",
		Ratio:    "9:16",
		Duration: upstream.CurrentDuration,
	}, ident, ""); err != nil {
		step("%s 提交失败: %v", strings.ToUpper(family), err)
		return false
	}
	step("%s 提交成功（键 %s），重新读取配额…", strings.ToUpper(family), key)

	// Upstream updates the counter synchronously, but a short pause keeps the
	// probe honest on a busy backend.
	select {
	case <-ctx.Done():
		return false
	case <-time.After(1200 * time.Millisecond):
	}

	after, err := up.Quota(ctx, key, "")
	if err != nil {
		step("%s 配额复核失败: %v", strings.ToUpper(family), err)
		return false
	}
	r := after.Remaining
	*remainingOut = &r

	if before == nil {
		step("%s remaining=%d（缺少基线，无法判定）", strings.ToUpper(family), after.Remaining)
		return false
	}
	decremented := after.Remaining < before.Remaining
	step("%s 提交后 remaining %d -> %d", strings.ToUpper(family), before.Remaining, after.Remaining)

	// Control read: a *different*, untouched key of the same family. If upstream
	// ignored the header and fell back to the real egress IP, the control key
	// would share that bucket and also show a decrement. Only a decrement on the
	// used key with a full control key proves the header drives the allowance.
	controlKey := controlAddress(family)
	control, err := up.Quota(ctx, controlKey, "")
	if err != nil {
		step("%s 对照读取失败: %v", strings.ToUpper(family), err)
		return false
	}
	controlIntact := control.Remaining >= control.Limit
	step("%s 对照键 %s remaining=%d/%d（%s）", strings.ToUpper(family), controlKey,
		control.Remaining, control.Limit,
		map[bool]string{true: "未被扣减，说明配额按 XFF 计", false: "也被扣减，说明配额并非按 XFF 计"}[controlIntact])

	if !decremented {
		step("%s 结论：XFF 未作为配额键（本次提交未扣减该键）", strings.ToUpper(family))
		return false
	}
	if !controlIntact {
		step("%s 结论：配额键不是 XFF（对照键同样被扣减）", strings.ToUpper(family))
		return false
	}
	step("%s 结论：✅ 上游以 X-Forwarded-For 作为独立配额键", strings.ToUpper(family))
	return true
}

// controlAddress returns a fresh, unused address of the given family to use as a
// control key.
func controlAddress(family string) string {
	if family == model.FamilyIPv6 {
		return identity.RandomIPv6(identity.PoolReserved, false)
	}
	return identity.RandomIPv4(identity.PoolReserved)
}
