package maxminddb

import (
	"bytes"
	"net/netip"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/oschwald/maxminddb-golang/v2/mmdbdata"
)

type characterizationCursorRecord struct {
	boolean  bool
	uint16   uint64
	entries  int
	cursorOK bool
}

func (r *characterizationCursorRecord) UnmarshalMaxMindDBCursor(
	cursor mmdbdata.Cursor,
) (mmdbdata.Cursor, error) {
	entries, err := cursor.Map()
	if err != nil {
		return mmdbdata.Cursor{}, mmdbdata.NormalizeUnmarshalError[characterizationCursorRecord](err)
	}

	var successor mmdbdata.Cursor
	for {
		key, valueCursor, ok := entries.Next(successor)
		if !ok {
			break
		}

		switch string(key) {
		case "boolean":
			r.boolean, successor, err = valueCursor.ReadBool()
		case "uint16":
			r.uint16, successor, err = valueCursor.ReadUint()
		default:
			successor, err = valueCursor.Skip()
		}
		if err != nil {
			return mmdbdata.Cursor{}, mmdbdata.NormalizeUnmarshalError[characterizationCursorRecord](err)
		}
		r.entries++
	}
	if err := entries.Err(); err != nil {
		return mmdbdata.Cursor{}, mmdbdata.NormalizeUnmarshalError[characterizationCursorRecord](err)
	}

	next, err := entries.End()
	if err != nil {
		return mmdbdata.Cursor{}, mmdbdata.NormalizeUnmarshalError[characterizationCursorRecord](err)
	}
	r.cursorOK = true
	return next, nil
}

func openCharacterizationReader(t *testing.T, name string) *Reader {
	t.Helper()
	reader, err := Open(testFile(name))
	require.NoError(t, err, "opening %s", name)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	return reader
}

func TestCharacterizationIPv4InIPv6SharesDataOffset(t *testing.T) {
	reader := openCharacterizationReader(t, "MaxMind-DB-test-mixed-24.mmdb")

	const address = "1.1.1.1"
	ipv4Result := reader.Lookup(mustParseCharacterizationAddr(address))
	mappedResult := reader.Lookup(mustParseCharacterizationAddr("::ffff:1.1.1.1"))
	aliasedResult := reader.Lookup(mustParseCharacterizationAddr("2001:0:101:101::"))

	require.True(t, ipv4Result.Found(), "IPv4 lookup should find the canonical IPv4 subtree")
	require.True(t, mappedResult.Found(), "IPv4-mapped IPv6 lookup should enter the IPv4 subtree")
	require.True(t, aliasedResult.Found(), "writer IPv4 alias should resolve to the same data record")
	require.Equal(t, ipv4Result.Offset(), mappedResult.Offset(),
		"IPv4 and ::ffff:0:0/96 lookups must resolve to one search-tree data pointer")
	require.Equal(t, ipv4Result.Offset(), aliasedResult.Offset(),
		"the writer's 2001:0:101:101:: alias also resolves through the same pointer")
	require.Equal(t, "1.1.1.1/32", ipv4Result.Prefix().String())
	require.Equal(t, "::ffff:1.1.1.1/128", mappedResult.Prefix().String())
	require.Equal(t, "2001:0:101:101::/64", aliasedResult.Prefix().String())

	var first struct {
		IP string `maxminddb:"ip"`
	}
	require.NoError(t, ipv4Result.Decode(&first))
	require.Equal(t, "::1.1.1.1", first.IP, "fixture stores the IPv6-shaped inserted address")

	first.IP = "mutated decoded value"
	var second struct {
		IP string `maxminddb:"ip"`
	}
	require.NoError(t, mappedResult.Decode(&second), "a shared offset remains independently decodable")
	require.Equal(t, "::1.1.1.1", second.IP,
		"decoding again is not affected by the caller-owned value from the first Result")
}

func TestCharacterizationDecodePathEmptyPathDecodesWholeRecord(t *testing.T) {
	reader := openCharacterizationReader(t, "MaxMind-DB-test-decoder.mmdb")
	result := reader.Lookup(mustParseCharacterizationAddr("1.1.1.1"))
	require.True(t, result.Found())

	var whole map[string]any
	require.NoError(t, result.DecodePath(&whole))
	require.Len(t, whole, 12)
	require.Equal(t, true, whole["boolean"])
	require.Equal(t, []any{uint64(1), uint64(2), uint64(3)}, whole["array"])

	var boolean bool
	require.NoError(t, result.DecodePath(&boolean, "boolean"))
	require.True(t, boolean)
	whole["boolean"] = false
	require.True(t, boolean, "path decoding owns its destination and does not alias a previous decoded map")
}

func TestCharacterizationNotFoundResultLivesWithoutReaderBacking(t *testing.T) {
	reader := openCharacterizationReader(t, "MaxMind-DB-test-mixed-24.mmdb")
	result := reader.Lookup(mustParseCharacterizationAddr("2001:db8::1"))

	require.False(t, result.Found())
	require.NoError(t, result.Err())
	require.Equal(t, uintptr(notFound), result.Offset())
	require.Equal(t, "2001:db8::/32", result.Prefix().String())

	var before any
	require.NoError(t, result.Decode(&before), "a tree-terminal empty result is a successful no-op")
	require.Nil(t, before)
	require.NoError(t, reader.Close())

	var after string
	require.NoError(t, result.Decode(&after), "empty results do not need Reader backing after Close")
	require.Empty(t, after)
}

func TestCharacterizationCursorUnmarshalerReceivesReaderBackedCursor(t *testing.T) {
	reader := openCharacterizationReader(t, "MaxMind-DB-test-decoder.mmdb")
	result := reader.Lookup(mustParseCharacterizationAddr("1.1.1.1"))
	require.True(t, result.Found())

	var record characterizationCursorRecord
	require.NoError(t, result.Decode(&record))
	require.True(t, record.cursorOK, "CursorUnmarshaler must return the proven complete-map successor")
	require.Equal(t, 12, record.entries)
	require.True(t, record.boolean)
	require.Equal(t, uint64(100), record.uint16)
}

func TestCharacterizationFoundResultKeepsOffsetAndClosedReaderRejectsDecode(t *testing.T) {
	reader, err := OpenBytes(readCharacterizationFile(t, "MaxMind-DB-test-ipv4-24.mmdb"))
	require.NoError(t, err)
	result := reader.Lookup(mustParseCharacterizationAddr("1.1.1.1"))
	require.True(t, result.Found())
	offset := result.Offset()

	var record struct {
		IP string `maxminddb:"ip"`
	}
	require.NoError(t, result.Decode(&record))
	require.Equal(t, "1.1.1.1", record.IP)

	require.NoError(t, reader.Close())
	require.Equal(t, "1.1.1.1", record.IP, "the previously decoded Go value remains caller-owned after Close")
	require.Equal(t, offset, result.Offset(), "Result retains its integer offset after Reader.Close")
	require.Equal(t, "1.1.1.1/32", result.Prefix().String(), "Prefix uses stored IP/prefix data after Close")

	var afterClose struct {
		IP string `maxminddb:"ip"`
	}
	err = result.Decode(&afterClose)
	require.EqualError(t, err, "cannot call Decode on a closed database")
	require.Empty(t, afterClose.IP, "failed post-Close decoding must leave the destination unchanged")

	err = result.DecodePath(&afterClose, "ip")
	require.EqualError(t, err, "cannot call DecodePath on a closed database")
	require.Empty(t, afterClose.IP)
}

func TestCharacterizationDataPointerOutOfBoundsIsDetectedAtLookup(t *testing.T) {
	buffer := readCharacterizationFile(t, "MaxMind-DB-test-ipv4-24.mmdb")
	reader, err := OpenBytes(buffer)
	require.NoError(t, err)
	metadataStart := bytes.LastIndex(buffer, metadataStartMarker)
	require.NotEqual(t, -1, metadataStart)
	searchTreeSize := int(searchTreeSizeBytes(reader.Metadata.NodeCount, reader.Metadata.RecordSize))
	dataSectionLen := metadataStart - searchTreeSize - dataSectionSeparatorSize
	require.NoError(t, reader.Close())

	badPointer := uint(dataSectionLen) + reader.Metadata.NodeCount + dataSectionSeparatorSize
	buffer[0] = byte(badPointer >> 16)
	buffer[1] = byte(badPointer >> 8)
	buffer[2] = byte(badPointer)

	reader, err = OpenBytes(buffer)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	result := reader.Lookup(mustParseCharacterizationAddr("0.0.0.1"))

	require.False(t, result.Found())
	require.Equal(t, uintptr(0), result.Offset(), "a rejected pointer must not expose a data-section offset")
	require.EqualError(t, result.Err(), "the MaxMind DB file's search tree is corrupt")
	var record any
	require.EqualError(t, result.Decode(&record), "the MaxMind DB file's search tree is corrupt")
	require.Nil(t, record)
}

func TestCharacterizationPointerFanOutAndOutOfBoundsFixture(t *testing.T) {
	fanout := openCharacterizationReader(t, "MaxMind-DB-test-pointer-decoder-dos.mmdb")
	result := fanout.Lookup(mustParseCharacterizationAddr("1.2.3.4"))
	offset := result.Offset()
	require.True(t, result.Found())
	require.NoError(t, result.Err())
	require.Equal(t, uintptr(235), offset)

	var value any
	err := result.Decode(&value)
	require.ErrorContains(t, err, "maximum decoded record size")
	decoded, ok := value.([]any)
	require.True(t, ok, "partial decode should retain the dynamic slice allocated before the budget error")
	require.Len(t, decoded, 2, "reflection publishes container growth as decoding proceeds")
	require.Equal(t, offset, result.Offset(), "the located pointer survives a later budget-limited decode failure")

	verifiedFanout := buildCharacterizationVerifiableFanoutFixture(t)
	result = verifiedFanout.Lookup(mustParseCharacterizationAddr("160.0.0.0"))
	require.Equal(t, uintptr(235), result.Offset())
	require.NoError(t, result.Err())

	var verifiedValue any
	err = result.Decode(&verifiedValue)
	t.Cleanup(func() { require.NoError(t, verifiedFanout.Close()) })
	require.ErrorContains(t, err, "maximum decoded record size")
	require.ErrorContains(t, verifiedFanout.Verify(), "maximum decoded record size",
		"Verify scans the reachable data section and budgets every top-level record independently")

	outOfBounds := buildCharacterizationOutOfBoundsFixture(t)
	require.EqualError(t, outOfBounds.Verify(), "the MaxMind DB file's search tree is corrupt",
		"the tree verifier resolves biased search-tree pointers before data decoding")
}

func TestCharacterizationPayloadBudgetIsPerDecodeAndIndependentOfVerify(t *testing.T) {
	reader := openCharacterizationReader(t, "MaxMind-DB-test-decoder-payload-limit.mmdb")
	result := reader.Lookup(mustParseCharacterizationAddr("1.2.3.4"))
	require.True(t, result.Found())

	var first [][]byte
	require.NoError(t, result.Decode(&first), "fixture is exactly at the 2 MiB payload boundary")
	require.Len(t, first, 33)
	var payloadBytes int
	for _, item := range first {
		payloadBytes += len(item)
	}
	require.Equal(t, 2<<20, payloadBytes)

	var second [][]byte
	require.NoError(t, result.Decode(&second), "each Result.Decode receives a fresh payload allowance")
	require.Len(t, second, len(first))
	err := reader.Verify()
	require.EqualError(t, err, "description - Expected: non-empty map Actual: map[]",
		"this raw writer fixture exercises decode budgets but is not a Verify-valid database")
}

func mustParseCharacterizationAddr(address string) netip.Addr {
	return netip.MustParseAddr(address)
}

func readCharacterizationFile(t *testing.T, name string) []byte {
	t.Helper()
	buffer, err := os.ReadFile(testFile(name))
	require.NoError(t, err, "reading %s", name)
	return bytes.Clone(buffer)
}

func buildCharacterizationOutOfBoundsFixture(t *testing.T) *Reader {
	t.Helper()

	buffer := readCharacterizationFile(t, "MaxMind-DB-test-ipv4-24.mmdb")
	metadataStart := bytes.LastIndex(buffer, metadataStartMarker)
	require.NotEqual(t, -1, metadataStart)

	probe, err := OpenBytes(buffer)
	require.NoError(t, err)
	searchTreeSize := int(searchTreeSizeBytes(probe.Metadata.NodeCount, probe.Metadata.RecordSize))
	dataSectionLen := metadataStart - searchTreeSize - dataSectionSeparatorSize
	badPointer := uint(dataSectionLen) + probe.Metadata.NodeCount + dataSectionSeparatorSize
	require.NoError(t, probe.Close())

	// Point node zero's left child one byte beyond the data section. The tree
	// record itself remains readable, but the pointer violates the ownership
	// boundary checked by Reader.resolveDataPointer.
	buffer[0] = byte(badPointer >> 16)
	buffer[1] = byte(badPointer >> 8)
	buffer[2] = byte(badPointer)

	reader, err := OpenBytes(buffer)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	return reader
}

func buildCharacterizationVerifiableFanoutFixture(t *testing.T) *Reader {
	t.Helper()
	source := readCharacterizationFile(t, "MaxMind-DB-test-ipv4-24.mmdb")
	markerOffset := bytes.LastIndex(source, metadataStartMarker)
	require.NotEqual(t, -1, markerOffset)

	metadata := bytes.Clone(source[markerOffset+len(metadataStartMarker):])
	nodeCountValue := append([]byte{0x4a}, []byte("node_count")...)
	nodeCountValue = append(nodeCountValue, 0xc1, 0xa3)
	nodeCountAt := bytes.Index(metadata, nodeCountValue)
	require.NotEqual(t, -1, nodeCountAt)
	metadata[nodeCountAt+len(nodeCountValue)-1] = 0x7f

	data := []byte{0xa0}
	offsets := []int{0}
	previousOffset := 0
	for range 40 {
		offset := len(data)
		data = append(data,
			0x02, 0x04,
			0x20|byte(previousOffset>>8), byte(previousOffset),
			0x20|byte(previousOffset>>8), byte(previousOffset),
		)
		offsets = append(offsets, offset)
		previousOffset = offset
	}

	const nodeCount = 127
	tree := make([]byte, 0, nodeCount*6)
	appendRecord := func(pointer uint) {
		tree = append(tree, byte(pointer>>16), byte(pointer>>8), byte(pointer))
	}
	for node := range nodeCount {
		if node < 63 {
			left := uint(node*2 + 1)
			appendRecord(left)
			appendRecord(left + 1)
			continue
		}

		leaf := node - 63
		if leaf >= len(offsets) {
			appendRecord(nodeCount)
			appendRecord(nodeCount)
			continue
		}
		pointer := uint(nodeCount + dataSectionSeparatorSize + offsets[leaf])
		appendRecord(pointer)
		appendRecord(pointer)
	}

	buffer := append([]byte{}, tree...)
	buffer = append(buffer, make([]byte, dataSectionSeparatorSize)...)
	buffer = append(buffer, data...)
	buffer = append(buffer, metadataStartMarker...)
	buffer = append(buffer, metadata...)

	reader, err := OpenBytes(buffer)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	return reader
}
