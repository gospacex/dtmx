package dtmx

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// tracerName 与 dbx 的 gorm callback 用同一个 instrumentation 名，
// 这样两处 span 在 Jaeger 里可按同一 instrumentation 归类。
const tracerName = "github.com/gospacex/dbx/orm"

// db.* 的 op 取值，**逐字对齐 dbx/orm/gorm_tracing.go** 的四个 callback。
// 验收断言（trace_e2e_test.go 的 ④）按 `db.` 前缀筛 span，故口径不能各写各的。
const (
	opCreate = "create"
	opQuery  = "query"
	opUpdate = "update"
	opDelete = "delete"
)

// dbOpOf 从 SQL 语句推出 db.operation 的取值。
//
// **为什么 op 必须由 SQL 推导、而不是让调用方声明**：第一版把 op 做成了必填参数，
// 结果四域 20 处调用点全传成了 `OpQuery` —— 调用点拿到的是 `query`、`dtmLogSQL`
// 这类**变量名**，不是 SQL 字面量，按首字母判动词必然落到默认值；而 INSERT/UPDATE
// 被标成 `db.query`，比没有 span 更坏：Jaeger 上看不出写在哪，容量与慢查询分析
// 会把写负载算到读上。op 是 SQL 本身的属性，让调用方「声明」等于给错值开门，
// 故这里直接去掉这个参数。
func dbOpOf(query string) string {
	lower := strings.ToLower(strings.TrimSpace(query))
	// 跳过前导注释与空白，避免 /* 库存扣减 */ insert into … 被判成默认分支。
	for _, kw := range []struct {
		prefix string
		op     string
	}{
		{"insert into", opCreate},
		{"replace into", opCreate},
		{"update", opUpdate},
		{"delete from", opDelete},
		{"select", opQuery},
	} {
		if strings.HasPrefix(lower, kw.prefix) {
			return kw.op
		}
	}
	// 认不出来的语句不猜：按 query 记，宁可标签粗一点，也不要把写标成读以外的任何东西。
	return opQuery
}

// ExecTx 在 dtm barrier 事务上执行一条语句，并开一个与 dbx gorm callback 同形的
// `db.<op>` span。
//
// **为什么需要它**：dtm 的子事务屏障 `BranchBarrier.CallWithDB` 要的是
// `*sql.Tx` —— gorm 不支持外接事务，所以分支面的落库只能走裸 SQL，绕开了 dbx
// 挂在 gorm callback 上的埋点。结果是链路上有 `rpc.dtm.*` → `service.*` →
// `repository.*`，独独**没有 `db.*`**：看得到「这个域被调了」，看不到「它写了库」。
// 这一层正是「数据操作入链」的最后一环，补上它不必改任何验收断言。
//
// traceOn=false 时本件是空操作（只执行不埋点）：db 层开关与业务分层开关各管一段，
// 与 otelx/layer.Begin 的注释口径一致。
func ExecTx(ctx context.Context, tx *sql.Tx, query string, traceOn bool, args ...any) (sql.Result, error) {
	if tx == nil {
		return nil, errors.New("dtmx: ExecTx 需要 barrier 事务（*sql.Tx），nil 会让写入脱离事务")
	}
	if !traceOn {
		return tx.ExecContext(ctx, query, args...)
	}
	op := dbOpOf(query)
	ctx, span := otel.Tracer(tracerName).Start(ctx, "db."+op)
	defer func() {
		span.End()
	}()

	attrs := []attribute.KeyValue{
		attribute.String("db.operation", op),
		attribute.String("db.system", "mysql"),
		attribute.String("db.query.text", query),
	}
	if tbl := tableNameOf(query); tbl != "" {
		attrs = append(attrs, attribute.String("db.sql.table", tbl))
	}
	span.SetAttributes(attrs...)

	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

// QueryRowTx 在 barrier 事务上查一行并 Scan，同时开 `db.<op>` span。
//
// 为什么不直接用 `sql.Row`：`*sql.Row` 是惰性的 —— `QueryRowContext` 返回时
// 结果集还没读，span 若在那时就 End，覆盖不到真正的读取过程，Jaeger 上会出现
// 「span 已结束但库里还没查到」的错位。故把 Scan 一并纳入 span 生命周期。
//
// dest 至少要有一个可 Scan 的目标；用法与 `sql.Row.Scan` 完全一致。
func QueryRowTx(ctx context.Context, tx *sql.Tx, query string, traceOn bool, dest []any, args ...any) error {
	if tx == nil {
		return errors.New("dtmx: QueryRowTx 需要 barrier 事务（*sql.Tx），nil 会让查询脱离事务")
	}
	if len(dest) == 0 {
		return errors.New("dtmx: QueryRowTx 需要至少一个 Scan 目标")
	}
	if !traceOn {
		return tx.QueryRowContext(ctx, query, args...).Scan(dest...)
	}
	op := opQuery
	ctx, span := otel.Tracer(tracerName).Start(ctx, "db."+op)
	defer span.End()

	attrs := []attribute.KeyValue{
		attribute.String("db.operation", op),
		attribute.String("db.system", "mysql"),
		attribute.String("db.query.text", query),
	}
	if tbl := tableNameOf(query); tbl != "" {
		attrs = append(attrs, attribute.String("db.sql.table", tbl))
	}
	span.SetAttributes(attrs...)

	err := tx.QueryRowContext(ctx, query, args...).Scan(dest...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// tableNameOf 从 SQL 里取表名，供 db.sql.table 属性用。与 dbx 的解析同口径：
// 顶住反引号、schema 前缀与多行语句；取不到就返回空串（属性就不挂，不猜）。
func tableNameOf(query string) string {
	q := strings.TrimSpace(query)
	// 只看第一个语句，避免子查询/union 里的表名被误认成主表。
	if i := strings.Index(q, ";"); i > 0 {
		q = strings.TrimSpace(q[:i])
	}
	lower := strings.ToLower(q)

	var re *regexp.Regexp
	switch {
	case strings.HasPrefix(lower, "insert into"), strings.HasPrefix(lower, "replace into"):
		re = regexp.MustCompile(`(?is)^\s*(?:insert\s+into|replace\s+into)\s+` + "`?" + `([\w.]+)` + "`?")
	case strings.HasPrefix(lower, "update"):
		// 修饰词只认 MySQL 的这三个。写成 `[a-z_]+` 会把**表名本身**吃掉
		// （`update t_stock set …` → 抓到 "set"），因为表名恰好也匹配 [a-z_]+。
		re = regexp.MustCompile(`(?is)^\s*update\s+(?:(?:low_priority|ignore|only)\s+)?` + "`?" + `([\w.]+)` + "`?")
	case strings.HasPrefix(lower, "delete from"):
		re = regexp.MustCompile(`(?is)^\s*delete\s+from\s+` + "`?" + `([\w.]+)` + "`?")
	case strings.HasPrefix(lower, "select"):
		re = regexp.MustCompile(`(?is)^\s*select\b[\s\S]*?\bfrom\s+` + "`?" + `([\w.]+)` + "`?")
	case strings.HasPrefix(lower, "replace "):
		re = regexp.MustCompile(`(?is)^\s*replace\s+into\s+` + "`?" + `([\w.]+)` + "`?")
	default:
		return ""
	}
	m := re.FindStringSubmatch(q)
	if len(m) < 2 {
		return ""
	}
	// schema.table → table（dbx 的 db.sql.table 只到表这一层）
	if i := strings.LastIndex(m[1], "."); i >= 0 {
		return m[1][i+1:]
	}
	return m[1]
}
