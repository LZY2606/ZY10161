# Reader.Lookup 执行导览

本文按当前源码的实际控制流记录 `Reader.Lookup` 到压缩查找树、数据指针、数据游标和自定义解码的所有权链。它不是 API 概览；每个结论都定位到 `reader.go`、`result.go`、`traverse.go`、`verifier.go`、`mmdbdata/` 或 `internal/decoder/` 中的具体函数。配套测试在 `characterization_contract_test.go`。

## 1. 入口与所有权

```text
Open/OpenBytes (reader.go)
  ├─ OpenBytes: 解析 metadata marker，创建两个数据解码器视图
  │   ├─ 临时 decoder.DecodeWithBudget -> Reader.Metadata
  │   └─ Reader.decoder: 以数据节切片为 backing buffer
  ├─ Reader.buffer: 整个 MMDB；OpenBytes 由调用方保留切片所有权
  ├─ Reader.dataSectionSize: 树大小 + 16 字节分隔符之后的数据节长度
  ├─ Reader.nodeOffsetMult = record_size / 4
  └─ setIPv4Start -> traverseTree(zeroIP, 0, 96)

Reader.Lookup(ip) (reader.go)
  ├─ Reader.buffer == nil -> Result{err: closed-database error}
  ├─ lookupPointer(ip)
  │   ├─ 校验 netip.Addr 和 IP 版本
  │   ├─ traverseTree(ip, 0, 128)
  │   │   ├─ traverseTree24
  │   │   ├─ traverseTree28
  │   │   └─ traverseTree32
  │   └─ 将最终节点号分类
  ├─ pointer == 0 -> Result{offset: notFound}
  └─ pointer > nodeCount -> resolveDataPointer -> Result{reader, offset}
```

`OpenBytes` 不复制调用方的 `[]byte`；`Reader.decoder` 只持有数据节子切片，但树边界检查和分隔符检查仍使用完整 `Reader.buffer`。`Reader.Lookup` 在成功定位数据记录时把 `*Reader` 放入 `Result`，随后通过 `runtime.KeepAlive(r)` 保证调用期间 Reader 不被 cleanup 提前回收。

## 2. 压缩查找树

分发函数是 `Reader.traverseTree`。它只按 `Metadata.RecordSize` 选择：

- 24 位：`Reader.traverseTree24`，每个节点 6 字节，左右记录各 3 字节。
- 28 位：`Reader.traverseTree28`，每个节点 7 字节，经 `readNode28` 读取共享 nibble。
- 32 位：`Reader.traverseTree32`，每个节点 8 字节，左右记录各 4 字节。

三个函数的共同边界假设：

1. 入口先验证 `len(Reader.buffer)` 至少覆盖 `nodeCount * nodeOffsetMult`，失败返回 `bounds check failed during tree traversal`。
2. 普通查找最多调用到 128 个 IP bit；循环条件是 `i < stopBit && node < nodeCount`。
3. IPv4 输入不从 128 位 IPv6 表示的第 97 位重新走前缀，而是把当前节点设为 `Reader.ipv4Start`、位深度设为 `Reader.ipv4StartBitDepth`，然后读取 32 位。
4. `OpenBytes` 通过 `Reader.setIPv4Start` 预先定位 IPv6 库的 IPv4 子树；IPv4-only 库固定使用 96 位深度。
5. 记录大小不是 24、28、32 时，由 `Reader.traverseTree` 返回 unsupported record size。

`Reader.traverseTree` 返回的是“最后读到的节点号”和已消费 bit 数；它本身不区分空记录和数据记录。

## 3. 三类 Lookup 结果

| 结果 | 产生位置 | Result 状态 | `Found()` | 后续行为 |
|---|---|---|---|---|
| 树结束 | `Reader.traverseTree24/28/32` 的循环在节点号等于 `Metadata.NodeCount` 时停止 | 这是 traversal 层节点号，还不是 Result | 不适用 | `Reader.NetworksWithin` 和 `searchTreeWalker.verifyPointer` 直接按空节点处理 |
| 未找到 | `Reader.lookupPointer` 把树结束节点号转换为 `pointer == 0`，`Reader.Lookup` 再写为 `offset: notFound` | `offset == notFound`，`err == nil` | false | `Decode`/`DecodePath` 直接返回 nil，不触碰 backing data |
| 数据指针错误 | `Reader.resolveDataPointer` 发现偏置后的偏移低于最小值或不小于 `dataSectionSize` | `err` 为 corrupt search tree，公开 offset 为 0 | false | `Decode`/`DecodePath` 返回同一个 `Result.err` |
| 找到数据 | `Reader.lookupPointer` 看到最终节点号大于 `Metadata.NodeCount`，且 `resolveDataPointer` 成功 | `reader != nil`，`offset` 是数据节内偏移，`err == nil` | true | `Decode`/`DecodePath` 才读取数据节 |
| Lookup 前置错误 | closed Reader 由 `Reader.Lookup` 直接返回；无效 IP 或 IP 版本错误由 `Reader.lookupPointer` 返回 | `err` 已设置，公开 offset 为 0 | false | `Decode`/`DecodePath` 原样返回该错误 |

这里的“树结束”和“未找到”是两层表示：节点号恰好等于 `Metadata.NodeCount` 是 traversal 层的合法空节点；`Reader.lookupPointer` 把它转换成返回值 `pointer == 0`，`Reader.Lookup` 再写为 `Result.offset == math.MaxUint`。遍历器 `Reader.NetworksWithin` 直接处理同一个节点号；只有启用 `IncludeNetworksWithoutData` 才 yield `offset: notFound`。

如果最终节点号小于 `Metadata.NodeCount`，说明在停止位耗尽时仍停在内部节点。普通 `Lookup` 因最多走 128 bit 不产生这种路径；`Reader.NetworksWithin` 的显式 DFS 会在继续展开前检查位字节是否越界，并构造 `invalid search tree at <prefix>` 的 `Result.err`。`Reader.Verify` 则通过 `searchTreeWalker.verifyNode` 和 `verifyPointer` 把内部节点出现在 bit depth 128、循环和超过 128 bit 的路径作为结构错误。

`Reader.resolveDataPointer` 的数据所有权公式是：

```text
minPointer = Metadata.NodeCount + 16
dataOffset = treeRecordValue - minPointer
valid      = treeRecordValue >= minPointer && dataOffset < Reader.dataSectionSize
```

该函数只验证偏移位于数据节范围内，不验证控制字节、类型、指针链或 payload。后者发生在 `Result.Decode`、`Result.DecodePath`、`mmdbdata.Cursor` 或 `Reader.Verify` 的数据阶段。

## 4. 从 Result 到 Decoder 和 Cursor

```text
Result.Decode(v) (result.go)
  ├─ r.err != nil -> 返回 Lookup 阶段错误
  ├─ r.offset == notFound -> nil；v 保持不变
  ├─ r.reader == nil || r.reader.buffer == nil -> closed database
  └─ r.reader.decoder.Decode(r.offset, v)
       internal/decoder/reflection.go: (*ReflectionDecoder).Decode
       ├─ v implements mmdbdata.CursorUnmarshaler
       │    └─ Cursor.UnmarshalCursor -> UnmarshalMaxMindDBCursor(cursor)
       ├─ v implements legacy mmdbdata.Unmarshaler
       │    └─ acquireDecoder -> UnmarshalMaxMindDB(*Decoder)
       ├─ *any -> decodeAnyWithBudget
       └─ struct/map/slice/scalar -> decodeValue

Result.DecodePath(v, path...) (result.go)
  ├─ 与 Decode 相同的 err/notFound/closed 前置检查
  └─ r.reader.decoder.DecodePath(r.offset, path, v)
       internal/decoder/reflection.go: (*ReflectionDecoder).DecodePath
       ├─ 非空 path：导航开始前创建 bounded decoder
       ├─ 空 path：直接调用 decodePath
       │    └─ 最终动态值或容器仍在 decodeAnyWithBudget/decodeValue 中激活预算
       └─ path miss：decodePath 返回 nil，目标值保持不变
```

`mmdbdata.Cursor`、`mmdbdata.Decoder`、`mmdbdata.MapCursor`、`mmdbdata.MapReader` 和 `mmdbdata.SliceCursor` 是 `internal/decoder` 类型的公开别名，定义在 `mmdbdata/type.go`。接口契约在 `mmdbdata/interface.go`。因此调用链中的实际方法仍位于：

- `internal/decoder/cursor.go`: `Cursor.Map`、`MapCursor.Next`、`MapCursor.End`、`Cursor.ReadBool`、`Cursor.ReadUint`、`Cursor.Skip` 等。
- `internal/decoder/reflection.go`: `ReflectionDecoder.Decode` 对 `CursorUnmarshaler` 和旧 `Unmarshaler` 的分派。
- `internal/decoder/data_decoder.go`: 控制字节、MMDB data pointer、标量和 key 的底层读取。

自定义 `CursorUnmarshaler` 必须消费完整根值并把 proven successor 返回给 `Cursor.UnmarshalCursor`。在 `Reader` 解码期间得到的 cursor 由该 `Reader` 的 backing data 支持；`Result` 和已返回的 Go 值不能延长 mmap 的可关闭生命周期。

## 5. IPv4 子树、IPv4-in-IPv6 与 Networks

`Reader.setIPv4Start` 在打开 IPv6 库时调用 `Reader.traverseTree(zeroIP, 0, 96)`，记录第 96 bit 后到达的节点和实际深度。`Reader.Lookup` 遇到 IPv4 地址时直接跳到该节点，再消费 32 个 IPv4 bit。

`Result.Prefix` 对 IPv4 地址调用 `Result.ipv4PrefixLen`：当 `Reader.hasIPv4Subtree` 为真且总深度不小于 `Reader.ipv4StartBitDepth` 时减去该深度；否则返回 invalid zero prefix 表示该前缀不能作为普通 IPv4 网络解释。

`Reader.Networks` 按 `Metadata.IPVersion` 选择 `::/0` 或 `0.0.0.0/0`，实际逻辑都在 `Reader.NetworksWithin`：

1. IPv4 prefix 用 `v4ToV16` 映射到 `::/96`，停止位加 96。
2. 初始边界由 `Reader.traverseTree(ip, 0, stopBit)` 定位。
3. DFS 栈按节点号处理：等于 node count 表示空网络；大于 node count 调 `resolveDataPointer`；小于 node count 用 `readNodePairBySize` 展开左右孩子。
4. 默认遍历时，若 DFS 从 IPv4 子树外再次到达 `Reader.ipv4Start`，会跳过该 IPv4 alias；`IncludeAliasedNetworks` 关闭此去重。
5. 数据 `Result` 在 yield 前解析一次数据偏移；`SkipEmptyValues` 额外调用 `ReflectionDecoder.IsEmptyValueAt`。

## 6. Verify 的防线

```text
Reader.Verify (verifier.go)
  ├─ (*verifier).verifyMetadata
  │    ├─ bytes.LastIndex(metadataStartMarker)
  │    ├─ decoder.VerifyMetadata: 带预算解码完整 metadata，并校验 UTF-8
  │    └─ 版本、database_type、description、IPVersion、RecordSize、NodeCount 等约束
  └─ (*verifier).verifyDatabase
       ├─ verifySearchTree
       │    └─ searchTreeWalker.verifyNode/verifyPointer
       │         ├─ nodeStates 做 DAG memoization 和 cycle 检测
       │         ├─ bitDepth/path height 限制为 128
       │         ├─ readNodePairBySize 读取左右孩子
       │         └─ resolveDataPointer 收集可达数据偏移
       ├─ verifyDataSectionSeparator
       │    └─ 树后必须有 16 个零字节
       └─ reader.decoder.VerifyDataSection(offsets)
            internal/decoder/verifier.go
            ├─ 顺序扫描数据节中的每个 top-level value
            ├─ 每个 value 使用独立预算 decode 到 any
            ├─ 校验 UTF-8、offset 单调前进
            ├─ 每个扫描到的 top-level offset 必须由搜索树引用
            └─ 搜索树引用的 offset 集合必须全部被扫描到
```

`Verify` 与单次 `Decode` 的防御范围不同：

- `Decode` 的预算属于一次调用。它限制当前根值经容器、路径导航、重复指针目标和动态 key 导航产生的工作；下一次 `Result.Decode` 重新获得预算。
- reflection 的容器预算按声明 child slots 预留，字符串、字节和动态 map key 使用独立的精确 2 MiB payload allowance；预算不足返回 `maximum decoded record size`。
- 直接解到具体标量的某些 fast path 不按容器展开收费；这是当前实现保留的性能/兼容性取舍。
- 自定义 `mmdbdata.CursorUnmarshaler` 和旧 `mmdbdata.Unmarshaler` 控制自己的 cursor/decoder 遍历，reflection 不替 callback 收取内部预算；实现必须自行限界。
- `Verify` 不是简单“预先 Decode 一次”。它验证 metadata、树 DAG、128 bit 深度、循环、分隔符、数据编码、UTF-8、数据节布局和树到数据的可达性，并对每个 top-level value 重置预算。
- `Verify` 成功只覆盖调用时的 backing data；之后 mmap 文件或 `OpenBytes` 输入切片被改写，结果不再由旧的成功验证保证。

配套测试中的 `buildCharacterizationVerifiableFanoutFixture` 构造 127 个 IPv4 树节点和 41 个数据节 top-level values：叶子 `uint16`、40 层每层 array-of-two-pointers。树的 64 个叶子分别引用这些 top-level values，使 `VerifyDataSection` 的可达性检查可以通过到扇出数据，再由独立数据预算拒绝；这把“可达性/布局防线”和“单记录扩张预算”分开验证。越界 fixture 则只改一个 24 位树记录，使其指向数据节结束后一个字节，`resolveDataPointer` 在任何数据控制字节读取前拒绝。

## 7. Close 与生命周期

- `Reader.Close` 对 mmap backing 调 `munmap`，然后把 `Reader.buffer` 置 nil、清空 `Reader.decoder` 和 `dataSectionSize`。
- `Open` 的 mmap 路径在 `Reader.openMmap` 取得平台映射，成功后通过 `runtime.AddCleanup` 绑定 `mmapCleanup`；显式 `Reader.Close` 调用平台 `munmap`（Unix 实现在 `mmap_unix.go`，Windows 实现在 `mmap_windows.go`）并翻转 `hasMappedFile`，阻止 cleanup 重复解除映射。
- `Result` 是值类型，保存 `ip`、`prefixLen`、`offset`、`err` 和一个 `*Reader`。Close 不回写已经返回的 Result。
- Found result 的 `Offset` 和 `Prefix` 在 Close 后仍可从 Result 内部整数和 IP 数据得到；`Decode`/`DecodePath` 因需要 `Reader.buffer` 与 decoder，返回 closed database 错误且不写入目标。
- notFound result 的 `Decode`/`DecodePath` 在检查 Reader backing 前先检查 `offset == notFound`，因此 Close 后仍是成功 no-op。
- 已解码到调用方变量中的 Go 值由调用方拥有；Close 不回收普通 Go 字符串、整数、struct 或 map。但 cursor、legacy decoder 和 `Cursor.ReadBytes`/map key 返回的输入别名不得在 Close 后使用。

## 8. Characterization tests 与已记录差异

测试文件：`characterization_contract_test.go`。

| 测试 | 覆盖的当前契约 |
|---|---|
| `TestCharacterizationIPv4InIPv6SharesDataOffset` | IPv4、`::ffff:1.1.1.1` 和 writer IPv4 alias 在 `MaxMind-DB-test-mixed-24.mmdb` 中解析到同一数据 offset；Prefix 保留各自网络形态；重复解码生成独立 Go 值 |
| `TestCharacterizationDecodePathEmptyPathDecodesWholeRecord` | `DecodePath(&v)` 空路径完整解码根 map；随后非空路径解码不依赖或修改前一次动态 map |
| `TestCharacterizationNotFoundResultLivesWithoutReaderBacking` | 未命中 Result 的 offset 为 `notFound`，Close 后 Decode 仍 no-op |
| `TestCharacterizationCursorUnmarshalerReceivesReaderBackedCursor` | `CursorUnmarshaler` 经 `Cursor.Map`、`MapCursor.Next`、标量读取和 `End` 消费完整 map |
| `TestCharacterizationFoundResultKeepsOffsetAndClosedReaderRejectsDecode` | Found Result 的 offset/prefix 和旧 Go 值在 Close 后保留；新的 Decode/DecodePath 被拒绝且目标不变 |
| `TestCharacterizationDataPointerOutOfBoundsIsDetectedAtLookup` | 树记录指向数据节外时，错误在 Lookup 阶段固化，Result 不暴露数据 offset |
| `TestCharacterizationPointerFanOutAndOutOfBoundsFixture` | 40 层指针扇出定位成功但反射解码预算失败；可验证 fixture 的 `Verify` 在数据阶段拒绝；越界树指针由 `resolveDataPointer` 拒绝 |
| `TestCharacterizationPayloadBudgetIsPerDecodeAndIndependentOfVerify` | 精确 2 MiB payload 每次 Decode 重置；raw payload fixture 的空 description 会先被 Verify metadata 拒绝 |

可执行结果与若干直觉性描述不同，测试按实现固定如下：

1. `MaxMind-DB-test-decoder.mmdb` 根 map 当前有 12 个键，不是早期导览中按字段名单数得到的 14。
2. IPv4-only `MaxMind-DB-test-ipv4-24.mmdb` 中 `1.1.1.1` 的 `ip` 字符串是 `1.1.1.1`；混合库插入值保留 writer 写入的 `::1.1.1.1`。
3. `2001:db8::1` 在混合库未命中时，`Result.Prefix` 是遍历停止处的 `2001:db8::/32`，不是更大的包含前缀。
4. 动态解码在预算耗尽时可能已经发布部分 container growth；测试断言可观察到部分 `[]any`，不把它当作原子回滚。
5. `MaxMind-DB-test-pointer-decoder-dos.mmdb` 与 payload-limit raw fixture 带有空 description，适合直接测试 Decode 预算，但会先在 `Verify.verifyMetadata` 失败。用于 Verify 数据预算时使用测试内构造的可验证扇出 fixture。

## 9. 复杂度与兼容性

- `Reader.Lookup`：树深度最多 128 步，时间复杂度 O(128)，空间复杂度 O(1)；IPv4 查询从 `ipv4Start` 开始，最多消费 32 个 IPv4 bit。
- `OpenBytes`/`Open`：metadata 解码与数据节大小检查为一次性成本；之后 Lookup 不扫描数据节。
- `Result.Decode`/`DecodePath`：时间和空间由实际选择的根值或路径决定，受当前反射预算约束；自定义 cursor callback 的复杂度由实现负责。
- `NetworksWithin`：访问请求范围内可达树节点并 yield 数据网络；alias 去重使用当前节点位置判断，不依赖文件系统顺序或 map 遍历顺序。
- `Verify`：访问可达树节点，并顺序扫描数据节 top-level values；带 memoization 的树状态空间为 O(nodeCount)，数据验证使用每记录预算但需要扫描整个数据节。
- 这些测试不改变公开 API，不新增外部服务、真实时钟等待或目录遍历依赖；fixture 来自仓库声明的本地 `testdata` submodule，越界/扇出变体在测试内存字节中构造。
