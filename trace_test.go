package dtmx

import (
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestExecTxEmitsDbSpan 与 dbx 的 gorm callback **同形**：span 名 `db.<op>`，
// 属性 db.operation / db.system / db.sql.table / db.query.text。
//
// 为什么必须有这个件：dtm 的 barrier 只吃 `*sql.Tx`（gorm 无法外接事务），
// 于是分支面的落库走的是**裸 SQL**，绕开了 dbx 挂在 gorm 上的 callback ——
// 结果是链路上有 `rpc.dtm.*` → `service.*` → `repository.*`，独独**没有 `db.*`**。
// 那正是「数据操作入链」断在最后一层：看得到"这个域被调了"，看不到"它写了库"。
// 本件把那一层补回来，且口径与既有 dbx span 完全一致，验收断言无需为它开特例。
func TestExecTxEmitsDbSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	// nil tx 不该 panic —— 域服在 barrier 事务缺失时要拿到清晰错误。
	if _, err := ExecTx(t.Context(), nil, "update stocks set available=available-1 where id=1", true); err == nil {
		t.Fatal("nil tx 必须报错而不是 panic")
	}

	// 真事务（假驱动）：只需要语句执行过即可观察 span。
	tx, done := beginFake(t)
	defer done()
	_, err := ExecTx(t.Context(), tx, "update t_stock set available = available - ? where id = ?", true, 1, 1)
	if err != nil {
		t.Fatalf("ExecTx 执行失败：%v", err)
	}

	spans := rec.Ended()
	var dbSpan bool
	for _, s := range spans {
		if s.Name() != "db.update" {
			continue
		}
		dbSpan = true
		got := map[string]string{}
		for _, a := range s.Attributes() {
			got[string(a.Key)] = a.Value.Emit()
		}
		if got["db.operation"] != "update" {
			t.Errorf("db.operation = %q，want update", got["db.operation"])
		}
		if got["db.system"] != "mysql" {
			t.Errorf("db.system = %q，want mysql（口径须与 dbx 一致）", got["db.system"])
		}
		if got["db.sql.table"] != "t_stock" {
			t.Errorf("db.sql.table = %q，want t_stock（从 SQL 里解析表名）", got["db.sql.table"])
		}
	}
	if !dbSpan {
		t.Fatalf("未产出 db.update span，实际 span 名：%v", spanNames(spans))
	}
}

// 反向探针：把 ExecTx 里的 span 名从 "db."+op 改成 r.traceOn 的业务名（"repository"），
// 本用例必须红（span 名对不上，且 db.sql.table 属性也不再断言得到）。
func TestExecTxSpanNameIsDbPrefixed(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	tx, done := beginFake(t)
	defer done()
	if _, err := ExecTx(t.Context(), tx, "insert into t_log (id) values (?)", true, 7); err != nil {
		t.Fatalf("ExecTx 执行失败：%v", err)
	}
	for _, s := range rec.Ended() {
		if s.Name() == "db.create" {
			return
		}
	}
	t.Fatalf("缺 db.create span，实际：%v", spanNames(rec.Ended()))
}

// TestExecTxTraceOnFalse 不产 span：db 层的开关与业务分层开关各管一段
// （见 otelx/layer.Begin 的注释），traceOn=false 时本件必须是空操作。
func TestExecTxTraceOnFalse(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	tx, done := beginFake(t)
	defer done()
	if _, err := ExecTx(t.Context(), tx, "select available from t_stock where id = ?", false, 1); err != nil {
		t.Fatalf("ExecTx 执行失败：%v", err)
	}
	for _, s := range rec.Ended() {
		if len(s.Name()) >= 3 && s.Name()[:3] == "db." {
			t.Fatalf("traceOn=false 时不应产出 db span，实际：%v", spanNames(rec.Ended()))
		}
	}
}

// TestTableNameOf 表名解析要顶得住三种形态：反引号、schema 前缀、多行 SQL。
// spanNames 便于失败时把实际 span 名打出来。
func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name())
	}
	return out
}

func TestTableNameOf(t *testing.T) {
	cases := map[string]string{
		"update `stocks` set available=1":                         "stocks",
		"insert into stock_logs (product_id) values (?)":          "stock_logs",
		"select sold\n  from db.product_sales where product_id=1": "product_sales",
		"delete from orders":                                      "orders",
	}
	for sql, want := range cases {
		if got := tableNameOf(sql); got != want {
			t.Errorf("tableNameOf(%q) = %q，want %q", sql, got, want)
		}
	}
	if got := tableNameOf("set x = 1"); got != "" {
		t.Errorf("无表名的语句应返回空串，实际 %q", got)
	}
}

// TestExecTxRejectsNilTxWithoutPanic barrier 事务缺失时必须给清晰错误：
// 分支面拿到 nil 再 panic 会把整个 gRPC server 进程带下去，故障现场全丢。
func TestExecTxRejectsNilTxWithoutPanic(t *testing.T) {
	if _, err := ExecTx(t.Context(), nil, "update stocks set available=0", true); err == nil {
		t.Fatal("nil tx 必须报错")
	}
	if err := QueryRowTx(t.Context(), nil, "select 1", true, []any{new(int)}); err == nil {
		t.Fatal("nil tx 的查询也必须报错")
	}
	if err := QueryRowTx(t.Context(), nil, "select 1", true, nil); err == nil {
		t.Fatal("无 Scan 目标也必须报错（否则 span 白开一条）")
	}
}

// 反向探针：把 QueryRowTx 里的 Scan 挪到 span.End() 之后（即退回 *sql.Row 的惰性
// 形态），本文件其余用例不会红 —— 所以这条判据的价值在于提醒：Scan 必须在
// span 生命周期内，改动时留意。

// TestDbOpOfDerivesVerbFromSQL op 由 SQL 推导后，写语句不再可能被标成读。
// 第一版的回归正是这里：调用方拿变量名而非字面量，声明式 op 全落成 OpQuery，
// 于是 Jaeger 上 20 处 INSERT/UPDATE 全显示 db.query。
func TestDbOpOfDerivesVerbFromSQL(t *testing.T) {
	cases := []struct{ sql, want string }{
		{"insert into order_items(a) values(?)", "create"},
		{"INSERT INTO orders(a) VALUES(1)", "create"},
		{"replace into outbox(a) values(1)", "create"},
		{"update stocks set available=available-1 where id=?", "update"},
		{"UPDATE orders set status=? where id=?", "update"},
		{"delete from stock_logs where id=?", "delete"},
		{"select id from orders where order_no=?", "query"},
		{"  \n\t UPDATE stocks set available=0", "update"},
	}
	for _, c := range cases {
		if got := dbOpOf(c.sql); got != c.want {
			t.Errorf("dbOpOf(%q) = %q，want %q", c.sql, got, c.want)
		}
	}
}
