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
	"go.opentelemetry.io/otel/trace"
)

// tracerName 与 dbx 的 gorm callback 用同一个 instrumentation 名，
// 这样两处 span 在 Jaeger 里可按同一 instrumentation 归类。
const tracerName = "github.com/gospacex/dbx/orm"

// db.* 的 op 取值，**逐字对齐 dbx/orm/gorm_tracing.go** 的四个 callback。
// 验收断言（trace_e2e_test.go 的 ④）按 `db.` 前缀筛 span，故口径不能各写各的。
const (
	OpCreate = "create"
	OpQuery  = "query"
	OpUpdate = "update"
	OpDelete = "delete"
)

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
func ExecTx(ctx context.Context, tx *sql.Tx, op, query string, traceOn bool, args ...any) (sql.Result, error) {
	if tx == nil {
		return nil, errors.New("dtmx: ExecTx 需要 barrier 事务（*sql.Tx），nil 会让写入脱离事务")
	}
	if !traceOn {
		return tx.ExecContext(ctx, query, args...)
	}
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

// spanAttrsEnd 收尾时挂属性 —— 供调用点在拿到结果后补业务维度（如影响行数）。
// 单独导出是因为 ExecTx 的 span 随函数返回即结束，调用点若想补属性得自己开 span；
// 这里给出的是与 ExecTx 同形的一段，省得各处自己拼 tracer 名。
func spanAttrsEnd(ctx context.Context, span trace.Span, kvs ...attribute.KeyValue) {
	if span != nil && span.IsRecording() {
		span.SetAttributes(kvs...)
	}
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
