package dtmx

import (
	"testing"

	"go.opentelemetry.io/otel"
	otelprop "go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/otelx/propagation"
)

// TestPayloadRoundTripCarriesTraceparentAndFailInject 是「分布式事务操作要入链」
// 的地基：traceparent 必须能穿过 dtm 的分支载荷往返。
//
// 为什么必须走载荷而不是让 dtm 透传：dtm server 只按 URL 回调，不带自定义
// metadata。正向链要挂回同一条 trace，只能由 bff 把 traceparent 编进业务载荷。
func TestPayloadRoundTripCarriesTraceparentAndFailInject(t *testing.T) {
	want := Payload{Biz: []byte(`{"order_no":"X1"}`), Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", FailInject: "points"}

	raw, err := Encode(want)
	if err != nil {
		t.Fatalf("Encode 失败：%v", err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode 失败：%v", err)
	}
	if got.Traceparent != want.Traceparent {
		t.Fatalf("traceparent 未往返：want %q got %q", want.Traceparent, got.Traceparent)
	}
	if got.FailInject != want.FailInject {
		t.Fatalf("fail_inject 未往返：want %q got %q", want.FailInject, got.FailInject)
	}
	if string(got.Biz) != string(want.Biz) {
		t.Fatalf("业务体未往返：want %s got %s", want.Biz, got.Biz)
	}
}

func TestDecodeBadJSONIsError(t *testing.T) {
	if _, err := Decode("{not json"); err == nil {
		t.Fatal("坏 JSON 必须报错，不能返回零值让分支面拿空载荷去落库")
	}
}

// TestExtractParentJoinsUpstreamTrace 与调用方的注入端同源：
// 编排侧用 Inject 编、分支面用 ExtractParent 挂回，两边必须是同一个全局传播器。
// 若这里裸调 otel 全局而装配期注册的是另一套传播器，链接不上 —— 而这种断裂
// 在 Jaeger 上表现为「每个服务都有 span，就是不在一条链上」，极难排查。
func TestExtractParentJoinsUpstreamTrace(t *testing.T) {
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	// 必须先按装配期的样子注册全局传播器：Inject/Extract 都经 otel 全局传播器
	// 工作，没注册时两者都退化成 no-op，本用例就会「因为测不到东西而绿」——
	// 正是最坏的那种假绿。这里不能调 launchx 的 telemetry.Init（组件仓不得
	// 依赖 launchx），故直接注册与装配期等价的传播器组合。
	otel.SetTextMapPropagator(otelprop.NewCompositeTextMapPropagator(
		otelprop.TraceContext{}, otelprop.Baggage{},
	))

	ctx := propagation.Extract(t.Context(), otelprop.MapCarrier{InjectKey: tp})
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		t.Fatalf("Extract 未解析出有效 SpanContext —— Inject/Extract 两端传播器不同源")
	}
	if got := sc.TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("TraceID 不符：want 4bf92f3577b34da6a3ce929d0e0e4736 got %s", got)
	}

	// 空 traceparent（dtm 异步补偿 / 重启续推）必须原样放行：
	// 这类回调按「新根 + biz.dtm.gid 关联」口径处理，不能凭空造一个孤儿 span。
	if got := trace.SpanContextFromContext(ExtractParent(t.Context(), "")).IsValid(); got {
		t.Fatal("空 traceparent 不应产出有效 SpanContext（异步补偿按新根处理）")
	}
	// 往返对称性：ExtractParent 挂回的 context，再 Inject 出去应得回同一个 traceparent。
	// bff 注入 → 域服挂回 →（域服内部再往下游注入）这条链路靠这个对称性成立。
	var carrier otelprop.MapCarrier = otelprop.MapCarrier{}
	propagation.Inject(ExtractParent(t.Context(), tp), carrier)
	if got := carrier.Get(InjectKey); got != tp {
		t.Fatalf("Extract→Inject 往返不对称：want %q got %q", tp, got)
	}
}

// 反向探针：把 ExtractParent 里的 propagation.Extract 换成裸 otel 全局
// otel.GetTextMapPropagator().Extract，本用例在传播器被装配期改写时必红。
