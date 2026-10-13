# dtmx

dtm（[distributed transaction manager](https://github.com/dtm-labs/dtm)）的 Go 客户端公共件。

三件套，各自解决一件「搞错了会静默丢账」的事：

| 件 | 解决什么 | 搞错的后果 |
|---|---|---|
| `result.go` | 业务失败标记 | 自造哨兵 → dtm server 认不出 → 业务失败（积分不足）被当基础失败**无限重试** |
| `payload.go` | traceparent 编解码与续链 | 续链入口与注入端不同源 → 每个服务都有 span 却不共链 |
| `barrier.go` | dtm 官方 `BranchBarrier` 包装 | 不套 barrier → 空补偿 / 重复回调 / 悬挂三条守卫全失效 |

## 硬约束

1. **不得自造失败哨兵。** `BusinessFailure` 包的是官方 `dtmcli.ErrFailure`，
   `IsBusinessFailure` 用 `errors.Is` 判。自造一个同文案错误 dtm server 认不出，
   业务失败就会被无限重试。
2. **barrier 表按域隔离。** 默认表名 `"barrier"`，落在**各域业务库内**，
   不是官方那个跨库的 `dtm_barrier.barrier` —— `BranchBarrier.CallWithDB` 是在
   **业务连接的事务内**插 barrier 行的，跨库就插不进这个事务，屏障等于没有。
   每个域服在装配期显式调 `SetBarrierTableName("barrier")`（该变量是进程级全局量，
   四域是四个独立进程，各设各的）。
3. **不得 import launchx / configx / dbx。** 本仓被 launchx 以
   `replace => ../dtmx` 消费，反向 import 会成环。由 `TestDtmxDoesNotDependOnLaunchx`
   守卫。续链走 `otelx/propagation`（launchx 的 `Inject`/`Extract` 背后即它）。
4. **`ExtractParent` 空 traceparent 原样放行。** dtm 的异步补偿 / 重启续推不带
   traceparent，这类回调按「新根 trace + `biz.dtm.gid` 关联」处理，
   凭空造同 ID 孤儿 span 是自欺。
5. **`Call` 的回调只准用 `tx` 落库。** 换成 gorm 或另开连接会脱离 barrier 的事务，
   守卫随之失效。

## 与 dtm 官方 API 的对应

| 本包 | dtm 官方 |
|---|---|
| `BusinessFailure` / `IsBusinessFailure` | `dtmcli.ErrFailure` |
| `SetBarrierTableName` | `dtmcli.SetBarrierTableName` |
| `BarrierFromCtx` | `dtmgrpc.BarrierFromGrpc` |
| `(*Barrier).Call` | `(*dtmcli.BranchBarrier).CallWithDB` |

编排侧（`dtmgrpc.NewSagaGrpc` / `TccGlobalTransaction` / `CallBranch`）**不在本包** ——
那是业务对事务形态的选择，不该由公共件替调用方决定。

## 分支寻址

Saga 的 action/compensate、TCC 的 try/confirm/cancel 都是 URL 字符串，形态
`<host:port>/<proto-service>/<method>`（无 scheme 前缀），由 dtm server 拆解后拨号。
本包**不生成**这些 URL —— host 段属调用方环境，由它从自己的配置拼入。

`<proto-service>` 必须与 busi.proto 里的 service 全名**逐字一致**；对不上时 dtm 回调
404，且只在 dtm server 日志里可见。

## 用法

```go
dtmx.SetBarrierTableName("barrier")              // 装配期，每进程一次

ctx = dtmx.ExtractParent(ctx, payload.Traceparent) // 分支面入口

bar, err := dtmx.BarrierFromCtx(ctx)              // 拿不到必须上抛
if err != nil {
    return err                                     // 忽略 = 无保护落库
}

err = bar.Call(ctx, rawDB, func(ctx context.Context, tx *sql.Tx) error {
    return store.DeductTx(ctx, tx, ...)            // 只准用 tx
})
if err != nil {
    if dtmx.IsBusinessFailure(err) {
        // 库存/积分不足等：dtm 不重试，立即转补偿
    }
    return err // 基础失败：dtm 按退避重试
}
```

gRPC 分支的失败还要落到 status code 上（业务失败 `codes.Aborted`、基础失败
`codes.Internal`）—— dtm 的分支回调用 raw codec，**从不解析响应体**，
只写 `resp.Code` 对 gRPC 分支无效。响应体只作人读与将来 HTTP 分支桥接。

## 依赖

`github.com/dtm-labs/client`（钉 v1.18.7）+ `github.com/gospacex/otelx` + otel。
构建：

```bash
go build ./... && go test ./...
```