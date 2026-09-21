# Lookup 执行链导览（reader / result / traverse / verifier / mmdbdata）

本文是当前实现的**可执行导览**，不是 API 概览。文中每一条结论都锚定到具体函数，
配套契约测试见仓库根目录 `lookup_walkthrough_test.go`；测试与本文冲突时，以测试的
可执行行为为准，差异记录在第 8 节。

- 锚定版本：本目录所在仓库当前工作树（package `maxminddb`，模块
  `github.com/oschwald/maxminddb-golang/v2`）。
- 涉及文件：`reader.go`、`result.go`、`traverse.go`、`verifier.go`、
  `mmdbdata/interface.go`、`mmdbdata/type.go`，以及内部包
  `internal/decoder`（`reflection.go`、`data_decoder.go`、`cursor.go`、
  `verifier.go`）。
- 测试使用的小型 fixture 全部来自 `testdata/test-data`（git submodule
  `test-data`，上游 maxmind/MaxMind-DB），无外网依赖。

## 1. 文件布局与 Reader 持有的状态

`OpenBytes`（`reader.go:338`）在打开时一次性确定以下边界，后续查找不再重算：

- `Metadata.NodeCount`、`Metadata.RecordSize`（24/28/32）、`Metadata.IPVersion`。
- `searchTreeSizeBytes(nodeCount, recordSize) = nodeCount * (recordSize/4)`
  （`reader.go:401`），即每节点 6/7/8 字节、每节点两条记录。
- 数据区起点 = 搜索树大小 + 16 字节全零分隔符（常量
  `dataSectionSeparatorSize = 16`，`reader.go:30`）；数据区终点 =
  `metadataStartMarker`（`"\xAB\xCD\xEFMaxMind.com"`）的偏移。`reader.decoder`
  只持有**数据区切片**（`reader.go:348`），因此 decoder 内 offset 是数据区相对偏移。
- `nodeOffsetMult = RecordSize/4`（每节点字节数），`reader.go:361`。
- `ipv4Start` / `ipv4StartBitDepth` 由 `(*Reader).setIPv4Start`（`reader.go:449`）
  在打开时计算（见第 4 节）。

搜索树记录值是**有偏指针**：

| 记录值 `p`       | 含义                                             |
| ---------------- | ------------------------------------------------ |
| `p < NodeCount`  | 指向树节点 `p`，继续走                           |
| `p == NodeCount` | 空记录（record is empty），查找结束，未找到      |
| `p > NodeCount`  | 数据指针，需减去 `NodeCount + 16` 得到数据区偏移 |

## 2. Lookup 的调用与所有权链

```
(*Reader).Lookup(ip netip.Addr) Result                    reader.go:407
  ├─ r.buffer == nil ?  → Result{err: "cannot call Lookup on a closed database"}
  ├─ (*Reader).lookupPointer(ip) (pointer, prefixLen, err) reader.go:471
  │    ├─ ip 有效性 / IPv6-in-IPv4-only 检查
  │    ├─ (*Reader).traverseTree(ip, 0, 128)              reader.go:560
  │    │    └─ 按 Metadata.RecordSize 分派：
  │    │         traverseTree24 / traverseTree28 / traverseTree32
  │    │         reader.go:576 / 647 / 726
  │    │         （28 位单记录读取走 readNode28，reader.go:719；
  │    │          Networks/Verify 成对读取走 readNodePairBySize，reader.go:502）
  │    └─ 终止节点分类：>NodeCount 数据；==NodeCount 空；<NodeCount 非法
  ├─ pointer == 0（空记录）→ Result{offset: notFound}（result.go const notFound）
  ├─ (*Reader).resolveDataPointer(pointer) (uintptr, error) reader.go:803
  └─ Result{reader: r, ip, offset, prefixLen, err}        result.go:12
        ├─ Result.Decode(v)                               result.go:45
        │    └─ r.decoder.Decode(offset, v)               internal/decoder/reflection.go:98
        │         ├─ v 实现 mmdbdata.CursorUnmarshaler
        │         │    → Cursor{callbackDataDecoder, offset}.UnmarshalCursor(v)
        │         ├─ v 实现（旧）mmdbdata.Unmarshaler
        │         │    → acquireDecoder(...); v.UnmarshalMaxMindDB(d); releaseDecoder(d)
        │         ├─ *any → decodeAnyWithBudget（带工作/载荷预算）
        │         └─ 其它具体类型 → decodeValue（标量有无预算快速路径）
        ├─ Result.DecodePath(v, path...)                  result.go:112
        │    └─ r.decoder.DecodePath(offset, path, v)     internal/decoder/reflection.go:160
        ├─ Result.Err / Found / Offset / Prefix           result.go:130/136/147/153
        └─ 每个出口的 runtime.KeepAlive(r)：防止 mmap 清理器在查找中途回收映射
```

**所有权要点**：

1. `Lookup` 只在压缩树里定位**数据区偏移**，不触碰数据内容；解码是惰性的，发生在
   之后的 `Decode` / `DecodePath`。
2. `Result` 是值类型，持有 `reader *Reader`、数据区相对偏移 `offset`、`ip`、
   `prefixLen`、`err`。它不拥有任何字节；字节归 `Reader.buffer`（mmap 或
   `OpenBytes` 调用方提供的切片）所有。
3. `runtime.KeepAlive(r)` 出现在 `Lookup`（`reader.go:412` 等）与
   `NetworksWithin`（`traverse.go`）尾部，以及 `Result.Decode/DecodePath`
   （`result.go:55`、`result.go:125`），保证当前调用期间 `runtime.AddCleanup`
   注册的 mmap 清理器（`reader.go:293`）不会提前 munmap。
4. 反射解码产生的标量/字符串/`[]byte` 写入调用方变量；其中 `[]byte` 经
   `(*DataDecoder).decodeBytes`（`internal/decoder/data_decoder.go:207`）
   **复制**（`make` + `copy`），字符串按 Go 语义也是独立副本。因此已解码的
   具体值在 `Close` 后仍归调用方所有。与之相对，低层
   `mmdbdata.Decoder.ReadBytes`（`internal/decoder/decoder.go:542`）返回的是
   **别名**输入缓冲的切片，文档明确要求复制后保留（`mmdbdata/type.go` 中
   `Decoder` / `Cursor` 注释）。

## 3. 三类查找结果何时产生

以返回的 `Result` 字段区分（`offset == notFound` 即 `math.MaxUint`，`result.go:10`）：

### 3.1 未找到（not found）—— `offset=notFound, err=nil`

产生位置：`(*Reader).lookupPointer`（`reader.go:471`）在遍历终止节点
`node == NodeCount` 时返回 `(0, prefixLen, nil)`（`reader.go:490` 附近
“Record is empty”），`Lookup` 随即构造 `Result{offset: notFound}`
（`reader.go:425`）。

可观察契约（`Result`，`result.go`）：

- `Err() == nil`，`Found() == false`（`result.go:136`：`err == nil && offset != notFound`）。
- `Offset()` 返回 `uintptr(math.MaxUint)`。
- `Decode(v)` / `DecodePath(v, ...)` 返回 `nil` 且**不改动 `v`**
  （`result.go:48`、`result.go:116` 的提前返回）。
- `Prefix()` 仍有定义：返回遍历实际停止处的前缀（可能是聚合网络），由
  `prefixLen` 决定，与是否找到数据无关（`result.go:153`）。
- IPv4 地址在 IPv6 库中的前缀长度换算走 `Result.ipv4PrefixLen`
  （`result.go:169`）：有 IPv4 子树时减去 `ipv4StartBitDepth`；没有时按
  96 位映射换算。

另一个“未找到”入口：`(*Reader).setIPv4Start` 阶段 IPv4-in-IPv6 映射网络
（如 `2001:200::/23`）的空记录同样落在该分支（fixture 实测，见第 8 节）。

### 3.2 树结束（tree ended / 结构非法）—— `err != nil`

这一类是遍历无法以“节点/空/数据”三态正常收尾：

- **无效 IP**：`lookupPointer` 对 `!ip.IsValid()` 返回
  `errInvalidIPAddress = "invalid IP address"`（`reader.go:33`、
  `reader.go:473`）。
- **IPv4-only 库查 IPv6**：`Metadata.IPVersion == 4 && ip.Is6()`
  （`reader.go:477`）。
- **终止节点小于 NodeCount**：遍历耗尽 128 位后仍停在内部节点，返回
  `InvalidDatabaseError("invalid node in search tree")`
  （`reader.go:496`）。fixture
  `MaxMind-DB-test-broken-search-tree-24.mmdb` 对 `128.128.128.128`、
  `255.255.255.255` 稳定触发该错误；注意该库根节点含自环，`Verify()`
  报的是 `invalid search tree: cycle at node 0`（第 6 节），两者是不同代码路径。
- **遍历缓冲区前置边界检查失败**：`traverseTree24/28/32` 开头要求
  `len(buffer) >= NodeCount*(6|7|8)`，否则返回
  `"bounds check failed during tree traversal"`（`reader.go:583`、
  `reader.go:655`、`reader.go:733`）。正常经 `Open/OpenBytes` 构造的 Reader
  在打开时已满足该条件，注释明确该分支只对“直接构造的 Reader（如测试）”可达。
- **遍历越界（仅 Networks/Verify）**：`NetworksWithin` 的 DFS 用
  `readNodePairBySize`（`reader.go:502`）成对读取，短缓冲返回
  `"bounds check failed: insufficient buffer for ... node pair read"`；
  位指针写爆 `As16` 时返回 `"invalid search tree at <prefix>"`
  （`traverse.go:219` 附近，参见 `TestNetworksWithInvalidSearchTree`）。

这些错误在 `Result` 上的表现：`err != nil`、`Found() == false`；`Decode` 与
`DecodePath` 的第一件事就是原样返回 `r.err`（`result.go:46`、`result.go:113`）。

### 3.3 数据指针错误 —— `err != nil`（resolveDataPointer）

`(*Reader).resolveDataPointer`（`reader.go:803`）只做两处算术边界校验：

1. `pointer < NodeCount + 16`（整数下溢保护）；
2. `resolved = pointer - (NodeCount+16)` 满足 `resolved < r.dataSectionSize`。

任一不满足都返回
`InvalidDatabaseError("the MaxMind DB file's search tree is corrupt")`。

触发方式：

- **查找路径**：树记录把某地址导向越界数据指针时，`Lookup` 返回的
  `Result.err` 已带该错误（`reader.go:433`），`Decode` 原样透传。
  fixture `MaxMind-DB-test-broken-pointers-24.mmdb` 中坏指针挂在
  `1.1.1.32/32`：`Networks(IncludeNetworksWithoutData())` 迭代到该网络时
  yield 该错误；普通地址（如 `1.1.1.1`）不受影响。
- **字节变异**：`reader_data_pointer_test.go` 的
  `TestLookupRejectsDataPointersOutsideDataSection` 直接改写
  `MaxMind-DB-test-ipv4-24.mmdb` 首条记录为越界值，得到同一错误。
- **迭代路径**：`NetworksWithin` 对 `pointer > NodeCount` 的节点同样调用
  `resolveDataPointer`，并把错误放进 yield 的 `Result.err`
  （`traverse.go:183`）。

注意：`resolveDataPointer` **只验证偏移范围，不验证目标控制字节/载荷**。
偏移落在数据区内但内容畸形时，错误推迟到 `Decode`（decoder）才产生；
`mmdbdata.Cursor.Offset()` 的注释同样声明 “Successful resolution does not
validate the value's payload”（`mmdbdata/type.go`）。

## 4. IPv4 子树：打开时定位，查找时跳转

`(*Reader).setIPv4Start`（`reader.go:449`）在 `OpenBytes` 末尾执行：

- `IPVersion != 6`：`ipv4StartBitDepth = 96`，`ipv4Start` 保持 0
  （IPv4-only 库，节点 0 就是 IPv4 根）。
- `IPVersion == 6`：对全零地址 `zeroIP = ::`（`reader.go:469`）调用
  `traverseTree(::, 0, 96)`，把前 96 位走完，得到 `(ipv4Start, depth)`。
  对规范库该 depth 为 96；当库没有 IPv4 搜索节点时，`ipv4Start` 可能等于
  `NodeCount`（空记录）或直接是数据指针——后者即
  `MaxMind-DB-no-ipv4-search-tree.mmdb` 的情形：所有 IPv4 地址及
  `::/96`、`::ffff:0:0/96` 映射都收敛到 `::/0` 区域里同一条记录（实测 offset 0，
  前缀 `::/64`）。

三个 `traverseTreeXX` 的 IPv4 快速路径结构一致（以
`traverseTree24` 为例，`reader.go:589`）：

1. `ip.Is4()` 时直接令 `i = ipv4StartBitDepth`、`node = ipv4Start`，跳过
   96 个 IPv6 前缀位；这就是“IPv4-in-IPv6”跳转，不需要逐位走映射前缀。
2. `stopBit <= i` 时立即返回（`NetworksWithin` 在 IPv4 前缀边界停止会用到）。
3. 32 个 IPv4 位打包进一个 `uint32`，每轮取最高位选左/右记录；28 位记录用
   `readNode28`（`reader.go:719`）一次 4 字节加载 + 移位拼出 28 位值。
4. IPv6 路径则按 32 位分块（`chunk := i >> 5`）从 `As16()` 取位。

实测契约（`MaxMind-DB-test-mixed-24.mmdb`，IPv6 库、24 位记录）：

| 查询地址                       | 走到的记录 | 结果                                    |
| ------------------------------ | ---------- | --------------------------------------- |
| `1.1.1.1`（IPv4）              | offset 54  | `1.1.1.1/32`，值 `{"ip":"::1.1.1.1"}`   |
| `::1.1.1.1`（映射位）          | offset 54  | 同一数据偏移，前缀 `::101:101/128`      |
| `::ffff:1.1.1.1`               | offset 54  | 同一数据偏移，前缀 `::ffff:1.1.1.1/128` |
| `2002:101:101::1`（6to4 别名） | offset 54  | 同一数据偏移，前缀 `2002:101:101::/48`  |
| `2001:200:0:1::1`              | 空记录     | not found，聚合前缀 `2001:200::/23`     |

`Result.Prefix()` 对 IPv4 `netip.Addr` 调 `ipv4PrefixLen`
（`result.go:169`）：有 IPv4 子树且 `prefixLen >= ipv4StartBitDepth` 时减去
该深度并返回 IPv4 前缀；否则返回映射网络的 IPv6 前缀。因此同一个 offset 54
会因查询地址族别不同报告不同前缀，但偏移与数据相同。

`Networks` 默认用
`node.pointer == r.ipv4Start && !isInIPv4Subtree(node.ip)` 跳过别名子树
（`traverse.go:177`），所以 mixed 库默认迭代 11 个网络，加
`IncludeAliasedNetworks()` 后为 29 个；`isInIPv4Subtree`（`traverse.go:267`）
用 `::255.255.255.255` 的下一地址 `::1:0:0 之前` 作边界，不硬编码具体别名网络。

## 5. 从 Result 到 mmdbdata Cursor：解码分派与所有权

`Result.Decode`（`result.go:45`）依次检查 `err`、`notFound`、关闭状态，然后
调用 `r.reader.decoder.Decode(offset, v)`。分派逻辑在
`(*ReflectionDecoder).Decode`（`internal/decoder/reflection.go:98`）：

1. **`mmdbdata.CursorUnmarshaler` 优先**：用 `callbackDataDecoder()`
   （`reflection.go:199`，Reader 发布前由 `PrepareForConcurrentUse`
   固定为共享的稳定 `DataDecoder`）构造
   `Cursor{decoder, offset}` 并调 `UnmarshalCursor`。
2. **旧 `mmdbdata.Unmarshaler`**：`acquireDecoder` 借出一个有状态
   `*Decoder`（池化），回调结束必须 `releaseDecoder`；文档明确回调不得保留
   decoder/iterator（`mmdbdata/interface.go`）。
3. **`*any`**：`decodeAnyWithBudget` 走带预算的动态解码（第 7 节）。
4. **直接具体类型的标量**：`tryFastDecodeUnbudgetedString/Bytes` 与
   `tryFastDecodeTyped` 等快速路径不带扩张预算（标量无法放大）。
5. 其余结构/具体容器：`decodeValue → decodeValueImpl`（`reflection.go:635+`），
   容器在 `reserveActiveContainer` / `reserveExactPayload` 处记账。

`Result.DecodePath`（`result.go:112`）→
`(*ReflectionDecoder).DecodePath`（`reflection.go:160`）：

- **空路径**：`path` 长度为 0，`decodePath`（`reflection.go:418`）的 `PATH`
  循环一次不执行，直接在根 offset 上按 `v` 的类型走与 `Decode` 相同的终端分派，
  即空路径等价于整记录解码；非空路径则在导航与最终值之间**共享同一组预算**
  （`reserveActiveContainer` 对每个途经容器扣账，跳过的键经
  `nextValueOffsetBudgeted` 处理但不跟随其指针目标）。
- 键/索引不存在时 `decodePath` 返回 `nil` 且不写 `v`（`reflection.go:470`
  “Not found”、越界索引同理）；判断存在性应解码到指针并检查 nil
  （`result.go` 的 `DecodePath` 文档给出了该用法）。

**Cursor 所有权与生命周期**（`mmdbdata/type.go` 的 `Cursor` 注释）：

- Reader 解码期间传入自定义 unmarshaler 的 cursor、其 successor，以及
  `MapReader`/`MapCursor`/`SliceCursor` 句柄都由该 Reader 的缓冲支撑；
  **不得在 `Close` 之后或与 `Close` 并发使用**（`reader.go` 包注释与
  `Reader` 类型注释同样声明）。
- 自定义 unmarshaler 必须消费完整值并返回“被证明的后继 cursor”；
  `UnmarshalCursor` 会校验后继（旧 `Unmarshal` 桥接需要重新扫描一次以推导后继）。
- 反射解码器的扩张/载荷预算**不进入**自定义回调；回调必须自行约束递归、
  迭代、重复指针目标、分配与字符串/字节产出（`mmdbdata/interface.go`）。

## 6. Networks / Verify / Close 共享的边界假设

### 6.1 Networks 与查找共用的原语

`NetworksWithin`（`traverse.go:91`）与 `Lookup` 共用：

- `traverseTree` 定位前缀内的起始节点（IPv4 前缀先经 `v4ToV16`
  （`traverse.go:272`）映射到 `::/96`，`stopBit += 96`）；
- `resolveDataPointer` 解析数据指针（`traverse.go:183`），错误语义与 Lookup
  完全一致；
- `mappedIP`（`traverse.go:259`）+ `v6ToV4`（`traverse.go:278`）负责把
  IPv4 子树内的 16 字节地址显示成 IPv4，与 `Result.Prefix()` 的换算假设一致；
- `NodeCount` 三态解释、`readNodePairBySize` 的成对边界检查。

差异：迭代是显式 DFS（`netNode` 栈），需要读两条记录而不是按 IP 取位，因此
边界检查集中在 `readNodePairBySize`；空记录默认跳过，加
`IncludeNetworksWithoutData()` 才 yield `offset: notFound` 的 Result；
`SkipEmptyValues()` 借助 `ReflectionDecoder.IsEmptyValueAt`
（`reflection.go:67`）在解码前探测空 map/array。

### 6.2 Verify 的三层防御

`(*Reader).Verify`（`verifier.go:115`）顺序执行：

1. `verifier.verifyMetadata`（`verifier.go:126`）：marker 存在性 +
   `decoder.VerifyMetadata`（`internal/decoder/verifier.go:12`，在**一组**
   预算下解码完整 metadata，含未知字段与指针目标）+ 版本/UTF-8/IPVersion/
   RecordSize/NodeCount 约束。
2. `verifier.verifyDatabase`（`verifier.go:213`）：
   - `verifySearchTreeWithStateCount`（`verifier.go:231`）：从根出发 DFS，
     `searchTreeWalker.verifyNode`（`verifier.go:28`）用 `nodeStates`
     记忆化（0 未访问 / 255 在栈上 / 已访问存高度），共享子树只走一次；
     在栈节点报 cycle，`bitDepth >= 128` 与高度超 128 报路径过长；
     `verifyPointer`（`verifier.go:78`）把数据指针经
     `resolveDataPointer` 收集为数据区 offset 集合。
   - `verifyDataSectionSeparator`（`verifier.go:267`）：16 字节必须全零。
   - `(*ReflectionDecoder).VerifyDataSection(offsets)`
     （`internal/decoder/verifier.go:23`）：从 offset 0 开始**线性**解码
     数据区，每个顶层值各自获得一份 `newBudgetedDecoder` 预算；要求
     (a) 每个顶层值起点必须在搜索树 offset 集合内（否则
     “found data (...) at N that the search tree does not point to”），
     (b) offset 严格前进，(c) 恰好走到数据区末尾，集合恰好清空。
3. 与 Lookup 共享 `runtime.KeepAlive`（`verifier.go:122`）。

### 6.3 payload budget 与 Verify 的防御范围为何不同

这是两种正交的防御，针对两种攻击面：

| 维度         | 反射解码预算（Decode/DecodePath）                                                                                                                                                                                       | Verify                                                                                             |
| ------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| 触发时机     | 每次 `Result.Decode`/`DecodePath`，只处理该记录                                                                                                                                                                         | 打开后主动全库扫描，与具体查询无关                                                                 |
| 作用对象     | 单条被查询记录的动态展开：32 768 个声明容器子槽（`decodeExpansionBudgetBytes = 2<<20`，单位 1/64，`data_decoder.go:120`）+ 独立的 2 MiB 物化字符串/字节/动态键载荷（`decodePayloadBudgetBytes`，`data_decoder.go:122`） | 树结构（环、深度、可达性）、分隔符、metadata；数据区按顶层值**逐个**给独立预算                     |
| 重复指针     | 同一操作内共享预算，指针扇出/载荷放大按展开成本累计扣账 → `errDecodedRecordTooLarge`（“exceeded maximum decoded record size”，`data_decoder.go:202`）                                                                   | 不展开“引用次数”；它要求每个顶层物理值只出现一次且被树直接引用，因此**结构可达性**而非预算是主防线 |
| 自定义解码   | 不约束 `CursorUnmarshaler`/`Unmarshaler` 回调内部（调用方自负）                                                                                                                                                         | 不执行用户回调，只用内部反射预算解码                                                               |
| 标量快速路径 | 直接具体类型标量无预算（不能放大）；`*any` 一律有预算                                                                                                                                                                   | 统一用 `decodeValue` + 预算解码                                                                    |

实测（fixture 由上游 writer 直接写字节构造，注释见
`testdata/pkg/writer/pointerdos.go`）：

- `MaxMind-DB-test-pointer-decoder-dos.mmdb`：40 层数组、每层两个指针指向下
  一层，未加防护的“按路径重复解码”成本是 2^40。`Lookup(1.2.3.4).Decode(&v)`
  在预算耗尽处失败并带完整路径上下文（`.../0/0/.../1/...`）；
  `Networks` 不受影响（不解析数据内容）。
- 同文件的 `Verify` 公开入口先在 `verifyMetadata` 失败（该合成库 metadata
  无 description，报 “description - Expected: non-empty map”）。白盒调用
  `verifier.verifyDatabase` 时，数据区第一个物理值（叶子
  `uint16 0`，位于 offset 0）不是搜索树直接引用的顶层值（树根只引用最外层
  数组），因此在**预算扣账之前**就被
  “found data ... that the search tree does not point to” 拒绝。
- 结论：**payload budget 防的是“一条记录被解码时的工作/字节放大”，Verify
  防的是“整库结构不合法”**；Verify 通过不代表解码无预算限制，Verify 失败也
  不一定是预算问题（本例先死在可达性/metadata）。对不受信任的库，应先
  `Verify` 且保持缓冲不变；即便如此，自定义/低层解码仍需调用方自行限量
  （`result.go` Decode 文档与 `mmdbdata/type.go` Cursor 文档均如此声明）。

## 7. mmap、Close 与 Result 生命周期

- `Open`（`reader.go:245`）在支持 mmap 的平台上映射文件；不支持（WASM、
  GAE）或文件系统拒绝时回退到整文件读入（`openFallback`，`reader.go:316`）
  再走 `OpenBytes`。平台分支：`mmap_unix.go` / `mmap_windows.go` /
  `mmap_stub.go`，本导览不改变任何平台行为。
- `hasMappedFile *atomic.Bool` 同时服务两条回收路径：
  - `(*Reader).Close`（`reader.go:323`）：CAS true→false 后 `munmap`，
    并清空 `buffer`、`decoder`、`dataSectionSize`；
  - `runtime.AddCleanup`（`reader.go:293`）：Reader 被 GC 且未显式 Close
    时兜底 munmap（同一 CAS 保证不会双重 unmap）。
- `Close` 之后的契约：
  - `Lookup`/`LookupOffset` 立即返回
    “cannot call Lookup/LookupOffset on a closed database”（`reader.go:409`、
    `reader.go:443`），因为 `buffer == nil`。
  - **Close 之前拿到的 `Result` 是值拷贝**，其 `offset`、`ip`、`prefixLen`、
    `err` 不依赖 Reader 是否存活：`Offset()`、`Prefix()`、`Err()`、`Found()`
    在 Close 后照常返回原值；但 `Decode`/`DecodePath` 因
    `r.reader.buffer == nil` 返回
    “cannot call Decode/DecodePath on a closed database”
    （`result.go:51`、`result.go:119`）。
  - 已经解码进调用方变量的具体值（含复制出的 `[]byte`/string）归调用方所有，
    Close 不回收它们；别名缓冲的低层 `Decoder.ReadBytes`/`ReadMapKey`
    切片除外（须自行复制）。
  - Reader-backed cursor 及其派生句柄在 Close 后不得使用
    （`mmdbdata/type.go`）。
  - `Close` 不得与任何 Reader/Result 方法或 cursor 使用并发
    （包注释、`Reader` 注释）；普通查找/解码彼此并发安全
    （`decoder.PrepareForConcurrentUse` 固化共享只读状态）。

## 8. 复杂度、兼容性取舍与实测差异记录

**复杂度**（N = NodeCount，D = 地址位数 ≤ 128，L = 记录序列化长度）：

- `Lookup`：O(D) 次节点读取，最坏 128（IPv4 快速路径 ≤ 32）；
  `resolveDataPointer` O(1)。不解码数据。
- `Decode`/`DecodePath`：O(L)，受扩张槽位与 2 MiB 载荷预算约束；
  `DecodePath` 的 map 键导航在单个 map 内为线性扫描。
- `Networks`：O(N + 数据指针数)，别名默认折叠；Verify：搜索树 DFS 记忆化
  O(N)，数据区线性扫描 O(数据区长度)，均为全库操作。
- `Close`：O(1)（munmap）。

**兼容性取舍**（本次不改公开 API，仅记录现状）：

- 24/28/32 三种 RecordSize 分派是固定的；`traverseTree` 对其它值返回
  “unsupported record size”，`OpenBytes` 接受的值由 metadata 决定而 Verify
  显式校验 24/28/32。
- 新代码应实现 `mmdbdata.CursorUnmarshaler`；旧 `mmdbdata.Unmarshaler`
  v2 全周期保留、v3 计划移除；两者并存时 cursor 版本优先
  （`reflection.go:102` 的分派顺序、`mmdbdata/interface.go`）。
- 预算数值（32 768 子槽 / 2 MiB 载荷 / 最大嵌套深度）是内部常量
  （`internal/decoder/data_decoder.go`），不是公开 API；`maxsize` 标签与
  `ReadStringMaxSize`/`SliceMaxSize`/`CheckMaxSize` 是公开的定点收窄手段。

**文档推导与现有测试/实测的差异（以可执行契约为准）**：

1. “Verify 会给每条**记录**独立预算”这一直观说法不准确：`VerifyDataSection`
   是按数据区**顶层物理值**顺序解码并各给一份预算；被指针引用的共享目标不是
   顶层值。指针扇出 fixture 因而先死于“search tree does not point to”可达性
   检查，而非预算。本文与 `lookup_walkthrough_test.go` 的
   `TestFanoutFixture_BudgetVsVerifyScope` 均按此实际顺序断言。
2. 合成 DoS fixture（`*-dos*.mmdb`、`*-payload-limit*.mmdb` 等）的 metadata
   刻意为空 description，公开 `Verify()` 一律先报 metadata 错误，**不能**用
   `Verify()==nil` 推导它们的数据区是否合法；这些 fixture 只服务于解码预算
   测试。导览中凡涉及它们，均区分公开 `Verify()` 与白盒 `verifyDatabase()`
   两层结果。
3. `MaxMind-DB-test-broken-search-tree-24.mmdb` 同时含根节点自环和越界
   终止节点：`Lookup(128.128.128.128)` 报 “invalid node in search tree”，
   `Verify()` 报 “cycle at node 0”，`Networks()` 报
   “invalid search tree at 128.128.128.128/32”。这三个结果来自三个不同函数
   （`lookupPointer`、`searchTreeWalker.verifyNode`、`NetworksWithin` 的位
   越界分支），不是同一个错误的三种包装。
4. not found 的 `Result.Offset()` 是 `MaxUint64`（`notFound`），不是 0；
   offset 0 是合法的数据区首值（如 `MaxMind-DB-test-pointer-decoder.mmdb`
   的 `1.0.0.0/32` 根记录就在 offset 0）。判断未找到必须用 `Found()` 或
   `Err()`，不能用 `Offset() == 0`。
5. 预算失败的动态解码（`*any`）**可能在目标里留下部分物化的值**：错误在展开
   中途返回时，已经写入的嵌套 slice/map 不会被回滚（实测
   `MaxMind-DB-test-pointer-decoder-dos.mmdb` 的 `Decode(&v)` 返回预算错误后
   `v` 非 nil）。拒绝信号是返回的 error；调用方不应在出错后复用该目标变量。
   这与 not found 的“成功且完全不写”契约不同（`result.go:48` 的提前返回）。
6. `MaxMind-DB-test-pointer-decoder.mmdb` 的两个根 map（offset 0 与 266）
   声明的条目数不同（15 与 12，含 `arrayX`/`mapXX` 等额外字段），但共享标量
   （如 `utf8_string`）经数据段指针去重后解析到同一控制字节（offset 247）。
   “共享指针”契约断言的是解析后偏移相等，不是根结构相同。

## 9. 配套测试索引

`lookup_walkthrough_test.go`（package `maxminddb` 内部测试，可直达小写函数）：

- `TestLookupWalkthrough_NotFoundTreeEndAndDataPointerErrors`：三类结果
  （未找到 / 树结束 / 数据指针错误）的字段级断言，对应第 3 节。
- `TestLookupWalkthrough_IPv4InIPv6Subtree`：mixed 库 IPv4 与
  `::a.b.c.d`、`::ffff:`、`2002:` 别名共享 offset、前缀换算与别名折叠/展开
  迭代计数，对应第 4 节。
- `TestLookupWalkthrough_NoIPv4SearchTree`：无 IPv4 搜索树库在
  `setIPv4Start` 阶段直接收敛到数据指针，IPv4 及映射地址共享 offset 0，
  对应第 4 节。
- `TestLookupWalkthrough_DecodePathEmptyPath`：空路径等于整记录解码、
  不存在路径不写目标，对应第 5 节。
- `TestLookupWalkthrough_SharedPointersShareResolvedOffset`：两个根记录经
  数据段指针共享同一标量控制字节（cursor `Offset()` 都解析到 247），对应
  第 1、5 节。
- `TestLookupWalkthrough_CursorUnmarshalerOwnership`：自定义 cursor 解码
  取数、后继交还与错误透传，对应第 5 节。
- `TestLookupWalkthrough_ResultOffsetLivesAfterClose`：Close 后偏移/前缀
  保留与解码失效、已解码副本仍可用的生命周期区分，对应第 7 节。
- `TestFanoutFixture_BudgetVsVerifyScope`：指针扇出 fixture 的解码预算
  错误 vs Verify 公开/白盒两层防御范围，对应第 6.3 节。

验收：`go test ./... -count=1`（环境准备 `go mod download`）。
