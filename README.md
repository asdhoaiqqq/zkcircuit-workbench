# 零知识电路工程工作台

## 用途

电路描述与版本、约束编译与校验、证明/验证作业编排、可信设置与聚合策略、产物版本冻结与复验。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `zkcircuit/`，命令入口位于 `cmd/zkcircuit/`。

```bash
go run ./cmd/zkcircuit demo
go run ./cmd/zkcircuit version
go test ./...
```

## 本地数据目录

数据命令通过 `--dir DIR` 指定一个本机数据目录（不存在则创建）。状态以带格式版本号的 JSON
信封原子提交到 `DIR/data.json`：每次写入先落临时文件 `data.json.tmp`、`fsync` 后 `rename`，
再对目录本身 `fsync`；操作期间用 `flock(2)` 做进程间互斥，进程内用互斥量串行化。
因此同一目录可被两个进程同时操作，成功返回的修改全部保留；写入途中被 SIGKILL 中断，
重开目录只能看到完整的旧状态或完整的新状态。损坏、截断或格式版本不受支持的文件会报
读取失败（退出码 3），原文件原样保留，不会被当作空目录覆盖。

目录内文件：

| 文件 | 作用 |
| --- | --- |
| `data.json` | 已提交状态（原子替换） |
| `data.json.tmp` | 下一次提交的暂存文件（崩溃残留会被忽略并替换） |
| `lock` | 跨进程互斥锁文件 |

## 命令

### 电路版本

电路以名称 + 正整数版本号共同标识，同名多版本可共存。

```bash
# 创建草稿版本（约束 > 0，输入数 >= 0，名称非空白）
zkcircuit circuit-create --dir DIR --name N --version V \
  [--constraints C] [--public-inputs P] [--private-inputs Q] [--description TEXT]

# 修改必须明确针对草稿：未知版本 not found，冻结版本 frozen
zkcircuit circuit-update --dir DIR --name N --version V \
  [--constraints C] [--public-inputs P] [--private-inputs Q] [--description TEXT]

# 冻结版本；重复冻结返回同一结果。冻结后名称/版本号/三个数量不可再改
zkcircuit circuit-freeze --dir DIR --name N --version V

# 查询：按名称字典序、版本升序
zkcircuit circuit-get  --dir DIR --name N --version V
zkcircuit circuit-list --dir DIR
```

创建已有版本：描述相同则幂等返回原记录；描述不同报 `conflict`（退出码 1），原记录不变。

### 约束定义、编译与输入检查

约束定义从 JSON 文件导入到**草稿**版本，编译只接受**冻结**版本，输入检查必须绑定一次编译产物的哈希。

定义文件形如：

```json
{
  "modulus": "7",
  "constraints": [
    { "a": [{"wire": 1, "coeff": "1"}], "b": [{"wire": 2, "coeff": "2"}], "c": [{"wire": 0, "coeff": "3"}] }
  ]
}
```

- `modulus` 是十进制字符串，必须是 2 至 2147483647 之间的质数（确定性 Miller–Rabin 判定）。
- 每条约束含有序的 `a`、`b`、`c` 三个项数组，每项是整数 `wire` + 十进制整数字符串 `coeff`，
  数组表示“系数乘对应编号值之和”，三者在模数下满足 a×b=c。编号 0 固定为常数 1，随后依次是
  声明的公开输入、私有输入；空数组表示 0，同一编号可重复（合并）。
- 约束条数必须等于版本声明的 `--constraints`，引用编号不得超出输入布局。
- 非法 JSON、非质数、缺字段、非法整数、越界编号或数量不符一律拒绝，**原定义保持不变**。

```bash
# 向草稿导入定义（整体替换）；版本不存在 not found，冻结版本 frozen
zkcircuit constraint-import --dir DIR --name N --version V --file def.json

# 仅冻结版本可编译；只登记数量、从未导入定义的版本明确报 "constraint definition missing"
zkcircuit circuit-compile  --dir DIR --name N --version V
# 输出: artifact: name=… version=… modulus=… constraints=… hash=<sha256>

# 检查输入必须显式给出编译产物哈希
zkcircuit input-check --dir DIR --name N --version V --hash HASH --file input.json
# 满足:   input check: satisfied=true  … hash=…
# 不满足: input check: satisfied=false … hash=… first_failure=<从1开始的首个失败约束>
```

产物哈希是规范化约束的 SHA-256，仅 JSON 空白、项顺序、重复项合并、零项增删或系数相差模数倍数时
保持不变；约束顺序、模数、名称、版本与公开/私有划分变化都会改变哈希。重复编译返回同一产物；新增
其他版本不影响已有产物。输入文件为 `{"public":[…],"private":[…]}` 两个字符串数组，长度须分别匹配
声明，值允许任意长度的带负号十进制整数；数组缺失、长度错误或值非法报 `input format error`。
检查绝不输出私有值，也不会建立证明作业。失败原因彼此可区分：版本未冻结 `not frozen`、缺少编译产物
`compiled artifact missing`、哈希不属于该版本 `artifact mismatch`。

草稿已有定义时，`circuit-update` 修改三个数量只有在定义仍然合法（约束数与编号布局都匹配）时才整体
生效，否则拒绝整次修改；冻结后定义不可替换。定义与编译产物随 `data.json` 持久化，重开目录后仍可用于
相同检查；旧数据目录可正常读取，新增记录损坏或产物与定义不一致时按读取失败拒绝（退出码 3），不覆盖
原文件。

### 可信设置

```bash
# 只属于指定的冻结版本；版本不存在 not found，尚未冻结 not frozen；重复登记幂等
zkcircuit setup-record --dir DIR --name N --version V
zkcircuit setup-get    --dir DIR --name N --version V
```

其他版本不能借用设置：为 `c@v1` 登记的设置对 `c@v2` 无效。

### 证明作业

只接受 `prove` 作业，登记不执行证明计算。失败原因可区分：版本不存在 `not found`、
尚未冻结 `not frozen`、该版本没有可信设置 `trusted setup missing`；拒绝不留作业。

```bash
# 编号非空白、电路名称明确、版本号正整数、尝试次数正整数
zkcircuit job-submit --dir DIR --id ID --name N --version V --attempt A [--kind prove]
zkcircuit job-get    --dir DIR --id ID
zkcircuit job-list   --dir DIR   # 按编号字典序
```

相同编号 + 相同请求重复提交返回同一条作业；编号相同内容不同报 `conflict`。
作业始终绑定提交时的电路版本，后来新增版本不改变查询结果。

### 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 1 | 业务规则失败（not found / conflict / frozen / not frozen / …） |
| 2 | 命令行用法错误 |
| 3 | 数据读取失败（损坏、截断、格式版本不受支持） |

## Go API

`zkcircuit` 包新增了持久化能力，但原有的 `Circuit` / `Job` 字段、`Validate` 与
`WitnessCost` 的调用方式保持不变：

```go
store, err := zkcircuit.Open("./bench-data")
// store.CreateCircuit / UpdateCircuit / FreezeCircuit / RecordSetup
// store.SubmitJob / GetJob / ListJobs / GetCircuit / ListCircuits / GetSetup
// store.ImportConstraints / GetDefinition / CompileCircuit / GetArtifact
// store.CheckInput / CheckInputFile
defer store.Close()
```

约束相关类型与错误：`Definition`/`Constraint`/`Term` 描述定义，`Artifact` 是编译产物，
`Witness` 与 `CheckResult` 用于输入检查；错误可按 `ErrDefinitionMissing`、
`ErrArtifactMissing`、`ErrArtifactMismatch`、`ErrInvalidInput` 用 `errors.Is` 区分。

## 技术方向

zk-snark, zk-circuit, gnark, plonk, groth16, verifiable-computation, zk-proof-aggregation

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
