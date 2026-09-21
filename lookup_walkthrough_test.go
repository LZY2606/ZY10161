// Characterization tests for the Lookup execution chain documented in
// docs/lookup-walkthrough.md. These tests pin current, executable behavior;
// they intentionally do not assert API comments. All fixtures come from the
// in-repo testdata submodule and every assertion carries the lookup address or
// operation that produced it, so a failure identifies the contract directly.
package maxminddb

import (
	"bytes"
	"math"
	"net/netip"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/oschwald/maxminddb-golang/v2/mmdbdata"
)

// openFixture opens a small test-data MMDB and registers Close as cleanup.
func openFixture(t *testing.T, name string) *Reader {
	t.Helper()
	reader, err := Open(testFile(name))
	require.NoError(t, err, "opening fixture %s", name)
	t.Cleanup(func() {
		// An already-closed reader returns no error here; individual tests may
		// close early, so do not require a successful second Close.
		_ = reader.Close()
	})
	return reader
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	addr, err := netip.ParseAddr(s)
	require.NoError(t, err, "parsing probe address %q", s)
	return addr
}

// TestLookupWalkthrough_NotFoundTreeEndAndDataPointerErrors pins the three
// result classes produced by (*Reader).lookupPointer, traverseTree*, and
// (*Reader).resolveDataPointer (docs section 3).
func TestLookupWalkthrough_NotFoundTreeEndAndDataPointerErrors(t *testing.T) {
	t.Run("empty record is notFound with zero-value decode", func(t *testing.T) {
		// MaxMind-DB-test-mixed-24.mmdb: 2001:200:0:1:: lives in an empty
		// branch whose aggregate prefix is 2001:200::/23.
		reader := openFixture(t, "MaxMind-DB-test-mixed-24.mmdb")

		result := reader.Lookup(mustAddr(t, "2001:200:0:1::1"))

		require.NoError(t, result.Err(), "empty record is not an error")
		assert.False(t, result.Found(), "empty record must not be Found")
		assert.Equal(t, uintptr(math.MaxUint64), result.Offset(),
			"notFound offset is math.MaxUint64, never 0")
		assert.Equal(t, "2001:200::/23", result.Prefix().String(),
			"Prefix() still reports the traversal stop prefix")

		var decoded map[string]any
		sentinel := map[string]any{"untouched": true}
		decoded = sentinel
		require.NoError(t, result.Decode(&decoded),
			"Decode on a notFound Result is a no-op success")
		assert.Equal(t, sentinel, decoded, "Decode must leave v unchanged")

		var viaPath string
		require.NoError(t, result.DecodePath(&viaPath, "ip"),
			"DecodePath on a notFound Result is also a no-op success")
		assert.Empty(t, viaPath, "DecodePath must not write on notFound")
	})

	t.Run("tree end below node count is invalid node error", func(t *testing.T) {
		// MaxMind-DB-test-broken-search-tree-24.mmdb terminates on an internal
		// node for high addresses: lookupPointer returns
		// "invalid node in search tree".
		reader := openFixture(t, "MaxMind-DB-test-broken-search-tree-24.mmdb")

		result := reader.Lookup(mustAddr(t, "128.128.128.128"))

		require.Error(t, result.Err())
		assert.ErrorContains(t, result.Err(), "invalid node in search tree")
		assert.False(t, result.Found())
		assert.Equal(t, uintptr(0), result.Offset(),
			"lookup errors carry the zero Result offset, not notFound")

		var decoded any
		err := result.Decode(&decoded)
		require.Error(t, err, "Decode replays the lookup error")
		assert.ErrorContains(t, err, "invalid node in search tree",
			"Decode must return the identical error category")
		assert.Nil(t, decoded)

		// A healthy address in the same fixture still resolves normally, which
		// proves the error is local to the traversed branch rather than a
		// whole-file open error.
		ok := reader.Lookup(mustAddr(t, "1.1.1.1"))
		require.NoError(t, ok.Err())
		assert.True(t, ok.Found())
	})

	t.Run("invalid address is rejected before traversal", func(t *testing.T) {
		reader := openFixture(t, "MaxMind-DB-test-ipv4-24.mmdb")

		result := reader.Lookup(netip.Addr{})
		require.EqualError(t, result.Err(), "invalid IP address")
		assert.False(t, result.Found())
		assert.Equal(t, uintptr(0), result.Offset())
	})

	t.Run("IPv6 address in IPv4-only database is rejected", func(t *testing.T) {
		reader := openFixture(t, "MaxMind-DB-test-ipv4-24.mmdb")

		result := reader.Lookup(mustAddr(t, "::1"))
		require.ErrorContains(t, result.Err(),
			"you attempted to look up an IPv6 address in an IPv4-only database")
		assert.False(t, result.Found())
	})

	t.Run("out-of-range data pointer corrupts exactly one branch", func(t *testing.T) {
		// MaxMind-DB-test-broken-pointers-24.mmdb carries one bad data pointer
		// at 1.1.1.32/32; resolveDataPointer rejects it while Networks yields
		// the error on that network and direct lookups of other addresses pass.
		reader := openFixture(t, "MaxMind-DB-test-broken-pointers-24.mmdb")

		var seenErrorAt string
		var healthyNetworks int
		for iterResult := range reader.Networks(IncludeNetworksWithoutData()) {
			if iterResult.Err() != nil {
				assert.ErrorContains(t, iterResult.Err(),
					"the MaxMind DB file's search tree is corrupt",
					"resolveDataPointer error propagated through NetworksWithin")
				seenErrorAt = iterResult.Prefix().String()
				continue
			}
			healthyNetworks++
		}
		assert.Equal(t, "1.1.1.32/32", seenErrorAt,
			"bad pointer is pinned to 1.1.1.32/32 in this fixture")
		assert.Positive(t, healthyNetworks,
			"other branches still iterate despite one corrupt pointer")

		// Verify classifies the same file at the structural layer; the exact
		// message comes from resolveDataPointer during tree walking.
		require.ErrorContains(t, reader.Verify(), "search tree is corrupt")
	})
}

// TestLookupWalkthrough_IPv4InIPv6Subtree pins the open-time IPv4 subtree jump
// (setIPv4Start + the traverseTree24/28/32 ip.Is4() fast path, docs section 4):
// IPv4 and its IPv6 mappings land on the same resolved data pointer but report
// address-family-specific prefixes.
func TestLookupWalkthrough_IPv4InIPv6Subtree(t *testing.T) {
	reader := openFixture(t, "MaxMind-DB-test-mixed-24.mmdb")

	type probe struct {
		addr        string
		wantFound   bool
		wantOffset  uintptr
		wantPrefix  string
		wantDecoded string
	}

	probes := []probe{
		{addr: "1.1.1.1", wantFound: true, wantOffset: 54,
			wantPrefix: "1.1.1.1/32", wantDecoded: "::1.1.1.1"},
		{addr: "::1.1.1.1", wantFound: true, wantOffset: 54,
			wantPrefix: "::101:101/128", wantDecoded: "::1.1.1.1"},
		{addr: "::ffff:1.1.1.1", wantFound: true, wantOffset: 54,
			wantPrefix: "::ffff:1.1.1.1/128", wantDecoded: "::1.1.1.1"},
		{addr: "2002:101:101::1", wantFound: true, wantOffset: 54,
			wantPrefix: "2002:101:101::/48", wantDecoded: "::1.1.1.1"},
	}

	firstResult := reader.Lookup(mustAddr(t, probes[0].addr))
	require.True(t, firstResult.Found())
	require.Equal(t, probes[0].wantOffset, firstResult.Offset())

	for _, probe := range probes {
		result := reader.Lookup(mustAddr(t, probe.addr))
		require.Equal(t, probe.wantFound, result.Found(), "%s Found", probe.addr)
		require.NoError(t, result.Err(), "%s lookup error", probe.addr)
		assert.Equal(t, probe.wantOffset, result.Offset(),
			"%s must resolve the same shared data offset as plain 1.1.1.1",
			probe.addr)
		assert.Equal(t, probe.wantPrefix, result.Prefix().String(),
			"%s prefix is family-specific even when data is shared", probe.addr)

		var record struct {
			IP string `maxminddb:"ip"`
		}
		require.NoError(t, result.Decode(&record), "%s decode", probe.addr)
		assert.Equal(t, probe.wantDecoded, record.IP,
			"%s decodes the aliased record payload", probe.addr)
	}

	// The IPv4-only record size variants use the same fast path; verify the
	// 28-bit and 32-bit readers use their ipv4Start values identically.
	for _, recordSizeFixture := range []string{
		"MaxMind-DB-test-mixed-28.mmdb",
		"MaxMind-DB-test-mixed-32.mmdb",
	} {
		sized := openFixture(t, recordSizeFixture)
		v4 := sized.Lookup(mustAddr(t, "1.1.1.1"))
		mapped := sized.Lookup(mustAddr(t, "::1.1.1.1"))
		require.True(t, v4.Found(), "%s IPv4 found", recordSizeFixture)
		require.True(t, mapped.Found(), "%s mapped IPv6 found", recordSizeFixture)
		assert.Equal(t, v4.Offset(), mapped.Offset(),
			"%s shares offsets across the IPv4 jump", recordSizeFixture)
		assert.NotEqual(t, v4.Prefix().String(), mapped.Prefix().String(),
			"%s still distinguishes displayed prefixes", recordSizeFixture)
	}

	// Alias folding is the NetworksWithin counterpart of the subtree jump:
	// the aliased locations are visited once by default and all when requested.
	assertNetworkCount := func(options []NetworksOption, want int, label string) {
		var count int
		for range reader.Networks(options...) {
			count++
		}
		assert.Equal(t, want, count, "%s network count", label)
	}
	assertNetworkCount(nil, 11, "default (aliases folded)")
	assertNetworkCount([]NetworksOption{IncludeAliasedNetworks()}, 29,
		"IncludeAliasedNetworks")
}

// TestLookupWalkthrough_NoIPv4SearchTree covers a database whose IPv4 mapped
// range resolves directly to data during setIPv4Start (ipv4Start is a data
// pointer rather than a tree node).
func TestLookupWalkthrough_NoIPv4SearchTree(t *testing.T) {
	reader := openFixture(t, "MaxMind-DB-no-ipv4-search-tree.mmdb")

	for _, addr := range []string{"1.1.1.1", "::1.1.1.1", "::ffff:1.1.1.1"} {
		result := reader.Lookup(mustAddr(t, addr))
		require.NoError(t, result.Err(), "%s", addr)
		assert.True(t, result.Found(), "%s", addr)
		assert.Equal(t, uintptr(0), result.Offset(),
			"%s lands on the single mapped record at data offset 0", addr)
		assert.Equal(t, "::/64", result.Prefix().String(),
			"%s reports the aggregate IPv6 prefix", addr)

		var value any
		require.NoError(t, result.Decode(&value))
		assert.Equal(t, "::/64", value, "%s decodes the shared ::/64 record", addr)
	}

	notFound := reader.Lookup(mustAddr(t, "2001:200:0:2::"))
	assert.False(t, notFound.Found())
	assert.NoError(t, notFound.Err())
	assert.Equal(t, "2000::/3", notFound.Prefix().String())
}

// TestLookupWalkthrough_DecodePathEmptyPath pins the empty-path contract in
// (*ReflectionDecoder).decodePath (docs section 5): an empty path performs a
// full-record decode through the same terminal dispatch as Decode, while a
// missing element leaves the destination untouched.
func TestLookupWalkthrough_DecodePathEmptyPath(t *testing.T) {
	reader := openFixture(t, "MaxMind-DB-test-decoder.mmdb")
	result := reader.Lookup(mustAddr(t, "::1.1.1.0"))
	require.True(t, result.Found())

	var whole map[string]any
	require.NoError(t, result.DecodePath(&whole),
		"empty DecodePath must decode the entire record")
	require.Len(t, whole, 12, "decoder fixture root has 12 keys")
	assert.Equal(t, true, whole["boolean"])
	assert.Equal(t, "unicode! ☯ - ♫", whole["utf8_string"])

	// Empty path into a concrete struct behaves like Decode too.
	var typed struct {
		Uint16 uint16 `maxminddb:"uint16"`
		Array  []uint `maxminddb:"array"`
	}
	require.NoError(t, result.DecodePath(&typed))
	assert.Equal(t, uint16(100), typed.Uint16)
	assert.Equal(t, []uint{1, 2, 3}, typed.Array)

	// A non-empty, existing path reaches the selected scalar.
	var first uint
	require.NoError(t, result.DecodePath(&first, "array", 0))
	assert.Equal(t, uint(1), first)

	// A missing map key returns nil and leaves the destination zero; existence
	// is observable through a pointer destination.
	var missing *string
	require.NoError(t, result.DecodePath(&missing, "does-not-exist"))
	assert.Nil(t, missing, "missing path leaves a nil pointer destination")

	var existing *string
	require.NoError(t, result.DecodePath(&existing, "utf8_string"))
	require.NotNil(t, existing)
	assert.Equal(t, "unicode! ☯ - ♫", *existing)

	// An out-of-range array index behaves the same as a missing key.
	var pastEnd uint
	require.NoError(t, result.DecodePath(&pastEnd, "array", 99))
	assert.Zero(t, pastEnd)

	// Empty path on a notFound Result stays a successful no-op rather than a
	// decode attempt at offset notFound.
	empty := reader.Lookup(mustAddr(t, "::2"))
	require.NoError(t, empty.Err())
	require.False(t, empty.Found())
	var untouched map[string]any = map[string]any{"kept": true}
	require.NoError(t, empty.DecodePath(&untouched))
	assert.Equal(t, map[string]any{"kept": true}, untouched)
}

// stringOffsetReader is a CursorUnmarshaler that resolves the control-byte
// offset of the "utf8_string" field instead of materializing every field. It
// proves that custom decoding runs on a Reader-backed Cursor and receives a
// proven-successor chain.
type walkthroughStringOffset struct {
	fieldOffset uint
	resolved    bool
	mapLen      uint
}

func (s *walkthroughStringOffset) UnmarshalMaxMindDBCursor(
	cursor mmdbdata.Cursor,
) (mmdbdata.Cursor, error) {
	mapCursor, err := cursor.Map()
	if err != nil {
		return mmdbdata.Cursor{}, err
	}
	s.mapLen = mapCursor.Size()

	keyCursor := mmdbdata.Cursor{}
	for {
		key, valueCursor, ok := mapCursor.Next(keyCursor)
		if !ok {
			break
		}
		if string(key) == "utf8_string" {
			offset, offsetErr := valueCursor.Offset()
			if offsetErr != nil {
				return mmdbdata.Cursor{}, offsetErr
			}
			s.fieldOffset = offset
			s.resolved = true
			next, skipErr := valueCursor.Skip()
			if skipErr != nil {
				return mmdbdata.Cursor{}, skipErr
			}
			keyCursor = next
			continue
		}
		next, skipErr := valueCursor.Skip()
		if skipErr != nil {
			return mmdbdata.Cursor{}, skipErr
		}
		keyCursor = next
	}
	return mapCursor.End()
}

// TestLookupWalkthrough_SharedPointersShareResolvedOffset uses the pointer
// decoder fixture: two distinct root records share deduplicated scalars via
// data-section pointers. Cursor.Offset resolves one pointer hop, so both roots
// report the same control-byte offset for utf8_string (docs sections 1 and 5).
func TestLookupWalkthrough_SharedPointersShareResolvedOffset(t *testing.T) {
	reader := openFixture(t, "MaxMind-DB-test-pointer-decoder.mmdb")

	first := reader.Lookup(mustAddr(t, "1.0.0.0"))
	second := reader.Lookup(mustAddr(t, "1.1.1.0"))
	require.True(t, first.Found())
	require.True(t, second.Found())
	require.NotEqual(t, first.Offset(), second.Offset(),
		"the two root maps sit at different data offsets (0 and 266)")

	var firstOffsets, secondOffsets walkthroughStringOffset
	require.NoError(t, first.Decode(&firstOffsets),
		"CursorUnmarshaler drives decoding for the first root record")
	require.NoError(t, second.Decode(&secondOffsets),
		"CursorUnmarshaler drives decoding for the second root record")

	require.True(t, firstOffsets.resolved)
	require.True(t, secondOffsets.resolved)
	// The two root maps declare different entry counts (15 and 10); the
	// contract under test is pointer deduplication, not equal shapes.
	assert.NotZero(t, firstOffsets.mapLen)
	assert.NotZero(t, secondOffsets.mapLen)
	assert.Equal(t, firstOffsets.fieldOffset, secondOffsets.fieldOffset,
		"pointer-deduplicated utf8_string resolves to the same control byte "+
			"from both root maps; expected 247 in this fixture")
	assert.Equal(t, uint(247), firstOffsets.fieldOffset,
		"pinned control-byte offset for the shared string")

	// The resolved offset is independently decodable through LookupOffset and
	// yields the shared scalar, demonstrating pointer semantics end to end.
	var shared string
	require.NoError(t, reader.LookupOffset(
		uintptr(firstOffsets.fieldOffset)).Decode(&shared))
	assert.Equal(t, "unicode! ☯ - ♫", shared)

	// Reflection decoding through both roots produces equal materialized
	// values even though the roots are separate maps.
	var firstMap, secondMap map[string]any
	require.NoError(t, first.Decode(&firstMap))
	require.NoError(t, second.Decode(&secondMap))
	assert.Equal(t, firstMap["utf8_string"], secondMap["utf8_string"])
	assert.Equal(t, firstMap["uint64"], secondMap["uint64"])
	assert.NotEqual(t, firstMap["array"], nil)
}

// cursorBool is a minimal handwritten CursorUnmarshaler over the decoder
// fixture's root map: it extracts "boolean" and skips every sibling, returning
// the map's proven successor.
type walkthroughCursorBool struct {
	value   bool
	seenKey string
}

func (b *walkthroughCursorBool) UnmarshalMaxMindDBCursor(
	cursor mmdbdata.Cursor,
) (mmdbdata.Cursor, error) {
	mapCursor, err := cursor.Map()
	if err != nil {
		return mmdbdata.Cursor{}, err
	}

	keyCursor := mmdbdata.Cursor{}
	for {
		key, valueCursor, ok := mapCursor.Next(keyCursor)
		if !ok {
			break
		}
		if string(key) == "boolean" {
			value, next, readErr := valueCursor.ReadBool()
			if readErr != nil {
				return mmdbdata.Cursor{},
					mmdbdata.NormalizeUnmarshalError[walkthroughCursorBool](readErr)
			}
			b.value = value
			b.seenKey = string(key)
			keyCursor = next
			continue
		}
		next, skipErr := valueCursor.Skip()
		if skipErr != nil {
			return mmdbdata.Cursor{}, skipErr
		}
		keyCursor = next
	}
	return mapCursor.End()
}

// TestLookupWalkthrough_CursorUnmarshalerOwnership pins the custom decoding
// path: ReflectionDecoder.Decode prefers CursorUnmarshaler, hands it a
// Reader-backed cursor, and accepts the returned successor (docs section 5).
func TestLookupWalkthrough_CursorUnmarshalerOwnership(t *testing.T) {
	reader := openFixture(t, "MaxMind-DB-test-decoder.mmdb")
	result := reader.Lookup(mustAddr(t, "::1.1.1.0"))
	require.True(t, result.Found())

	custom := &walkthroughCursorBool{}
	require.NoError(t, result.Decode(custom),
		"CursorUnmarshaler decodes without reflection")
	assert.Equal(t, "boolean", custom.seenKey)
	assert.True(t, custom.value, "the decoder fixture stores boolean=true")

	// Decoding the same Result repeatedly is safe: the Reader keeps the backing
	// data alive and each callback receives a fresh cursor at the offset.
	custom2 := &walkthroughCursorBool{}
	require.NoError(t, result.Decode(custom2))
	assert.Equal(t, custom.value, custom2.value)

	// A kind mismatch inside the callback surfaces through the same category
	// as reflection when NormalizeUnmarshalError is applied: decoding a boolean
	// field as a string via the cursor API becomes an UnmarshalTypeError-style
	// failure rather than a panic.
	var intoString string
	err := result.DecodePath(&intoString, "boolean")
	require.Error(t, err, "boolean cannot decode into string")

	// Legacy Unmarshaler remains supported on the same fixture (v2 contract).
	var legacy legacyBooleanReader
	require.NoError(t, result.Decode(&legacy))
	assert.True(t, legacy.value, "legacy Unmarshaler still works")
}

type legacyBooleanReader struct {
	value bool
}

func (b *legacyBooleanReader) UnmarshalMaxMindDB(d *mmdbdata.Decoder) error {
	entries, _, err := d.ReadMap()
	if err != nil {
		return err
	}
	for key := range entries {
		if string(key) == "boolean" {
			value, readErr := d.ReadBool()
			if readErr != nil {
				return readErr
			}
			b.value = value
			continue
		}
		if err := d.SkipValue(); err != nil {
			return err
		}
	}
	return nil
}

// TestLookupWalkthrough_ResultOffsetLivesAfterClose distinguishes the offset
// metadata retained by a Result value from the decoded value's lifetime
// (docs section 7). The offset is a number owned by the Result; decoding needs
// the Reader's live buffer, but copies already made into caller variables are
// caller-owned.
func TestLookupWalkthrough_ResultOffsetLivesAfterClose(t *testing.T) {
	reader := openFixture(t, "MaxMind-DB-test-decoder.mmdb")
	result := reader.Lookup(mustAddr(t, "::1.1.1.0"))
	require.True(t, result.Found())

	// Materialize values before Close: strings and reflection-decoded byte
	// slices are copies (DataDecoder.decodeBytes does make+copy), so they are
	// independent of the mmap/backing slice.
	var before struct {
		UTF8 string `maxminddb:"utf8_string"`
		Data []byte `maxminddb:"bytes"`
	}
	require.NoError(t, result.Decode(&before))
	require.Equal(t, "unicode! ☯ - ♫", before.UTF8)
	require.Equal(t, []byte{0x00, 0x00, 0x00, 0x2a}, before.Data)

	offsetSnapshot := result.Offset()
	prefixSnapshot := result.Prefix().String()

	// LookupOffset returns another Result bound to the same Reader; capture it
	// before Close to prove the error is about Reader state, not Result age.
	offsetResult := reader.LookupOffset(offsetSnapshot)
	require.NoError(t, offsetResult.Err())

	require.NoError(t, reader.Close())

	// The Result value retains its immutable navigation data after Close.
	assert.Equal(t, offsetSnapshot, result.Offset(),
		"Offset survives Close: it is stored in the Result value")
	assert.Equal(t, prefixSnapshot, result.Prefix().String(),
		"Prefix survives Close: it derives from stored ip/prefixLen/metadata")
	assert.True(t, result.Found(), "Found is computed from stored fields")
	assert.NoError(t, result.Err())

	// Any attempt to touch the backing bytes is rejected.
	var again string
	err := result.Decode(&again)
	require.ErrorContains(t, err, "cannot call Decode on a closed database")
	assert.Empty(t, again, "failed post-Close decode must not write the target")

	err = result.DecodePath(&again, "utf8_string")
	require.ErrorContains(t, err, "cannot call DecodePath on a closed database")

	var viaOffset []byte
	require.ErrorContains(t, offsetResult.Decode(&viaOffset),
		"cannot call Decode on a closed database")

	// New lookups fail at the Reader guard before traversal.
	postClose := reader.Lookup(mustAddr(t, "::1.1.1.0"))
	require.EqualError(t, postClose.Err(),
		"cannot call Lookup on a closed database")
	require.False(t, postClose.Found())

	// Caller-owned copies remain intact and usable; Close does not reclaim them.
	assert.Equal(t, "unicode! ☯ - ♫", before.UTF8)
	assert.Equal(t, []byte{0x00, 0x00, 0x00, 0x2a}, before.Data)
}

// TestFanoutFixture_BudgetVsVerifyScope explains why the reflection decode
// payload/expansion budget and (*Reader).Verify defend different surfaces
// (docs section 6.3). The fixture encodes 40 nested arrays, each holding two
// data-section pointers to the next level: a decoder that re-materialized
// every pointer target per referencing path would perform 2**40 leaf decodes
// from a 451-byte file.
func TestFanoutFixture_BudgetVsVerifyScope(t *testing.T) {
	const fanoutFile = "MaxMind-DB-test-pointer-decoder-dos.mmdb"

	t.Run("per-record decode rejects exponential fanout via shared budget", func(t *testing.T) {
		reader := openFixture(t, fanoutFile)
		result := reader.Lookup(mustAddr(t, "1.2.3.4"))
		require.NoError(t, result.Err(), "Lookup only locates the pointer")
		require.True(t, result.Found())
		require.Equal(t, uintptr(235), result.Offset(),
			"the outermost array sits at data offset 235")

		// Decoding into any charges every declared container child against one
		// operation budget; the error pinpoints the depth at which it trips and
		// names the shared limit rather than hanging on exponential work.
		var dynamic any
		err := result.Decode(&dynamic)
		require.Error(t, err)
		assert.ErrorContains(t, err, "exceeded maximum decoded record size")
		// Note the actual contract: a dynamic (*any) decode that trips the
		// budget may leave a partially materialized value in the destination.
		// The error is the rejection signal; callers must not reuse the target
		// after an error (see docs section 6.3).

		// The same per-record budget covers DecodePath, including the empty
		// path which is a whole-record decode.
		var viaPath any
		err = result.DecodePath(&viaPath)
		require.ErrorContains(t, err, "exceeded maximum decoded record size")

		// Tree navigation itself never decodes data, so Networks is unaffected
		// by the amplification shape: it resolves pointers arithmetically and
		// yields the networks without touching the arrays.
		var networks int
		for iterResult := range reader.Networks() {
			require.NoError(t, iterResult.Err())
			networks++
		}
		assert.Positive(t, networks, "iteration does not expand data pointers")
	})

	t.Run("Verify defends structure and reachability, not decode amplification", func(t *testing.T) {
		reader := openFixture(t, fanoutFile)

		// Public Verify fails at metadata validation first: the synthetic DoS
		// fixture deliberately ships an empty description map. That rejection
		// happens in verifyMetadata, before verifyDatabase ever runs.
		err := reader.Verify()
		require.Error(t, err)
		assert.ErrorContains(t, err, "description - Expected: non-empty map",
			"public Verify reports the metadata defect of the synthetic fixture")

		// Advancing to the database layer shows how VerifyDataSection actually
		// treats the fanout: it linearly decodes top-level physical values and
		// requires each one to be referenced directly by the search tree. The
		// leaf uint16 at offset 0 is only reachable through nested pointers, so
		// the reachability check rejects it before any per-record budget is
		// exhausted. Verify therefore catches this file structurally even
		// though it does not count 2**40 decodes.
		v := verifier{reader: reader}
		dbErr := v.verifyDatabase()
		require.Error(t, dbErr)
		assert.ErrorContains(t, dbErr,
			"that the search tree does not point to")
	})

	t.Run("out-of-range data pointer is rejected at resolve time and by Verify", func(t *testing.T) {
		// Build the corrupt case in memory: copy a valid small database and
		// overwrite the first node's left 24-bit record with a value that passes
		// the "> NodeCount" data-pointer test but resolves past the data
		// section. This exercises resolveDataPointer's upper-bound check
		// without depending on fixture generation or file ordering.
		original, err := readFixtureBytes(t, "MaxMind-DB-test-ipv4-24.mmdb")
		require.NoError(t, err)
		mutated := append([]byte(nil), original...)

		opened, err := OpenBytes(original)
		require.NoError(t, err)
		searchTree := searchTreeSizeBytes(opened.Metadata.NodeCount,
			opened.Metadata.RecordSize)
		markerOffset := indexMarker(mutated)
		require.NotEqual(t, -1, markerOffset)
		dataLen := markerOffset - int(searchTree) - dataSectionSeparatorSize
		require.Positive(t, dataLen)
		bad := uint(dataLen) + opened.Metadata.NodeCount + dataSectionSeparatorSize
		require.NoError(t, opened.Close())

		mutated[0] = byte(bad >> 16)
		mutated[1] = byte(bad >> 8)
		mutated[2] = byte(bad)

		corrupt, err := OpenBytes(mutated)
		require.NoError(t, err)
		t.Cleanup(func() { _ = corrupt.Close() })

		result := corrupt.Lookup(mustAddr(t, "0.0.0.1"))
		require.ErrorContains(t, result.Err(),
			"the MaxMind DB file's search tree is corrupt",
			"resolveDataPointer rejects a resolved offset >= dataSectionSize")
		assert.False(t, result.Found())

		var decoded any
		require.ErrorContains(t, result.Decode(&decoded),
			"the MaxMind DB file's search tree is corrupt")
		require.ErrorContains(t, corrupt.Verify(),
			"the MaxMind DB file's search tree is corrupt",
			"the search-tree walk applies the same resolveDataPointer bound")
	})
}

// readFixtureBytes loads a fixture as an independent byte slice so tests can
// mutate a copy without affecting other tests that Open the same path.
func readFixtureBytes(t *testing.T, name string) ([]byte, error) {
	t.Helper()
	return os.ReadFile(testFile(name))
}

func indexMarker(buffer []byte) int {
	return bytes.LastIndex(buffer, metadataStartMarker)
}
