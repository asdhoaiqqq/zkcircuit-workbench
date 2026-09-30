# 零知识电路工程工作台

## 用途

电路描述与版本、约束编译与校验、证明/验证作业编排、可信设置与聚合策略、产物版本冻结与复验。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `zkcircuit/`，命令入口位于 `cmd/zkcircuit/`。

```bash
go run ./cmd/zkcircuit demo
go run ./cmd/zkcircuit version
go test ./...
```

## 技术方向

zk-snark, zk-circuit, gnark, plonk, groth16, verifiable-computation, zk-proof-aggregation

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
