package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// The git object model and smart-HTTP protocol primitives: a faithful port of
// the reference git.ts. SHA-1 object ids, loose-object semantics over the
// in-memory store, packfile reading (OFS/REF delta resolution, thin-pack
// fix-up) and writing (full objects), pkt-line framing.

// --- pkt-lines ---

func pktLineBytes(body []byte) []byte {
	out := make([]byte, 0, len(body)+4)
	out = append(out, []byte(fmt.Sprintf("%04x", len(body)+4))...)
	out = append(out, body...)
	return out
}

func pktLine(text string) []byte { return pktLineBytes([]byte(text)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type pkt struct {
	kind    string // data | flush | delim | end
	payload []byte
}

func parsePktHeader(buffer []byte, at int) (int, error) {
	header := string(buffer[at : at+4])
	for _, c := range header {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return 0, fmt.Errorf("pkt-line header is not hex: %q", header)
		}
	}
	var length int
	_, err := fmt.Sscanf(header, "%x", &length)
	if err != nil {
		return 0, err
	}
	return length, nil
}

// parsePkts mirrors git.ts parsePkts: a flush ends the pkt portion; the rest
// is returned raw (the receive-pack packfile).
func parsePkts(buffer []byte) ([]pkt, []byte, error) {
	packets := []pkt{}
	at := 0
	for at < len(buffer) {
		if at+4 > len(buffer) {
			return nil, nil, errors.New("pkt-line header is truncated")
		}
		length, err := parsePktHeader(buffer, at)
		if err != nil {
			return nil, nil, err
		}
		if length == 0 {
			packets = append(packets, pkt{kind: "flush", payload: []byte{}})
			at += 4
			return packets, buffer[at:], nil
		}
		if length == 1 {
			packets = append(packets, pkt{kind: "delim", payload: []byte{}})
			at += 4
			continue
		}
		if length == 2 {
			packets = append(packets, pkt{kind: "end", payload: []byte{}})
			at += 4
			continue
		}
		if length < 4 || at+length > len(buffer) {
			return nil, nil, errors.New("pkt-line length is out of range")
		}
		packets = append(packets, pkt{kind: "data", payload: buffer[at+4 : at+length]})
		at += length
	}
	return packets, buffer[at:], nil
}

// parseAllPkts mirrors git.ts parseAllPkts (the stream must consume the buffer).
func parseAllPkts(buffer []byte) ([]pkt, error) {
	packets := []pkt{}
	at := 0
	for at < len(buffer) {
		if at+4 > len(buffer) {
			return nil, errors.New("pkt-line header is truncated")
		}
		length, err := parsePktHeader(buffer, at)
		if err != nil {
			return nil, err
		}
		if length == 0 {
			packets = append(packets, pkt{kind: "flush", payload: []byte{}})
			at += 4
			continue
		}
		if length == 1 {
			packets = append(packets, pkt{kind: "delim", payload: []byte{}})
			at += 4
			continue
		}
		if length == 2 {
			packets = append(packets, pkt{kind: "end", payload: []byte{}})
			at += 4
			continue
		}
		if length < 4 || at+length > len(buffer) {
			return nil, errors.New("pkt-line length is out of range")
		}
		packets = append(packets, pkt{kind: "data", payload: buffer[at+4 : at+length]})
		at += length
	}
	return packets, nil
}

// --- Trees, commits, tags ---

type TreeEntry struct {
	Mode string
	Name string
	Oid  string
}

func parseTree(data []byte) ([]TreeEntry, error) {
	entries := []TreeEntry{}
	at := 0
	for at < len(data) {
		space := bytes.IndexByte(data[at:], 0x20)
		if space < 0 {
			return nil, errors.New("tree entry has no mode terminator")
		}
		space += at
		mode := string(data[at:space])
		nulRel := bytes.IndexByte(data[space+1:], 0x00)
		if nulRel < 0 {
			return nil, errors.New("tree entry is truncated")
		}
		nul := space + 1 + nulRel
		if nul+21 > len(data) {
			return nil, errors.New("tree entry is truncated")
		}
		name := string(data[space+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, Oid: oid})
		at = nul + 21
	}
	return entries, nil
}

func mustParseTree(data []byte) []TreeEntry {
	entries, err := parseTree(data)
	if err != nil {
		return []TreeEntry{}
	}
	return entries
}

func treeSortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode + " " + e.Name + "\x00")
		raw, _ := hex.DecodeString(e.Oid)
		buf.Write(raw)
	}
	return buf.Bytes()
}

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) CommitView {
	text := string(data)
	split := strings.Index(text, "\n\n")
	header := text
	message := ""
	if split >= 0 {
		header = text[:split]
		message = text[split+2:]
	}
	view := CommitView{Parents: []string{}}
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			view.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			view.Parents = append(view.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			view.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			view.Committer = line[10:]
		}
	}
	view.Message = message
	return view
}

func buildCommit(tree string, parents []string, author, committer, message string) []byte {
	var sb strings.Builder
	sb.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		sb.WriteString("parent " + p + "\n")
	}
	sb.WriteString("author " + author + "\ncommitter " + committer + "\n\n")
	sb.WriteString(message)
	return []byte(sb.String())
}

type TagView struct {
	Object  string
	Type    string
	Tag     string
	Message string
}

func parseTag(data []byte) TagView {
	text := string(data)
	split := strings.Index(text, "\n\n")
	header := text
	message := ""
	if split >= 0 {
		header = text[:split]
		message = text[split+2:]
	}
	view := TagView{}
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			view.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			view.Type = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			view.Tag = strings.TrimSpace(line[4:])
		}
	}
	view.Message = message
	return view
}

func putObject(repo *Repo, objType string, data []byte) string {
	oid := hashObject(objType, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = &GitObject{Type: objType, Data: data}
	}
	return oid
}

// --- Packfiles ---

var typeIDs = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var idTypes = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func packObjectHeader(objType string, size int) []byte {
	out := []byte{}
	first := (typeIDs[objType] << 4) | (size & 0x0f)
	size /= 16
	if size > 0 {
		first |= 0x80
	}
	out = append(out, byte(first))
	for size > 0 {
		b := size & 0x7f
		size /= 128
		if size > 0 {
			b |= 0x80
		}
		out = append(out, byte(b))
	}
	return out
}

func writePack(objects []*GitObject) []byte {
	var body bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	header[7] = 2
	header[8] = byte(len(objects) >> 24)
	header[9] = byte(len(objects) >> 16)
	header[10] = byte(len(objects) >> 8)
	header[11] = byte(len(objects))
	body.Write(header)
	zw, _ := zlib.NewWriterLevel(&body, zlib.DefaultCompression)
	for _, object := range objects {
		body.Write(packObjectHeader(object.Type, len(object.Data)))
		zw.Reset(&body)
		zw.Write(object.Data)
		zw.Close()
	}
	trailer := sha1.Sum(body.Bytes())
	out := append(body.Bytes(), trailer[:]...)
	return out
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// inflateAt decompresses the zlib stream starting at offset and reports where
// it ended (the Go counterpart of Node's inflateSync with info).
func inflateAt(buffer []byte, offset int) ([]byte, int, error) {
	cr := &countingReader{r: bytes.NewReader(buffer[offset:])}
	br := bufio.NewReaderSize(cr, 4096)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		zr.Close()
		return nil, 0, err
	}
	if err := zr.Close(); err != nil {
		return nil, 0, err
	}
	consumed := cr.n - int64(br.Buffered())
	if consumed <= 0 {
		return nil, 0, fmt.Errorf("zlib stream at offset %d consumed no bytes", offset)
	}
	return data, offset + int(consumed), nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() (int, error) {
		value := 0
		shift := 0
		for {
			if offset >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			b := delta[offset]
			offset++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := 0
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					copyOffset += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					copySize += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			copy(out[written:], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			copy(out[written:], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, errors.New("delta produced the wrong size")
	}
	return out, nil
}

type packEntry struct {
	typ        string
	data       []byte
	baseOffset *int
	baseOid    *string
	isDelta    bool
	offset     int
}

func readPack(pack []byte, resolveExternal func(oid string) *GitObject) (map[string]*GitObject, error) {
	if len(pack) < 32 {
		return nil, errors.New("pack is too short")
	}
	if string(pack[0:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("pack version %d is not supported", version)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	checksum := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(checksum[:], pack[len(pack)-20:]) {
		return nil, errors.New("pack checksum mismatch")
	}
	entries := map[int]*packEntry{}
	order := []int{}
	at := 12
	for index := 0; index < count; index++ {
		start := at
		b := pack[at]
		at++
		typeID := (b >> 4) & 0x07
		size := int(b & 0x0f)
		shift := 4
		for b&0x80 != 0 {
			b = pack[at]
			at++
			size += int(b&0x7f) << shift
			shift += 7
		}
		var baseOffset *int
		var baseOid *string
		if typeID == 6 {
			bb := pack[at]
			at++
			value := int(bb & 0x7f)
			for bb&0x80 != 0 {
				bb = pack[at]
				at++
				value = (value+1)*128 + int(bb&0x7f)
			}
			v := start - value
			baseOffset = &v
		} else if typeID == 7 {
			s := hex.EncodeToString(pack[at : at+20])
			baseOid = &s
			at += 20
		}
		data, next, err := inflateAt(pack, at)
		if err != nil {
			return nil, err
		}
		at = next
		if len(data) != size {
			return nil, errors.New("pack object size mismatch")
		}
		isDelta := typeID == 6 || typeID == 7
		typ := "blob"
		if !isDelta {
			typ = idTypes[int(typeID)]
		}
		entries[start] = &packEntry{typ: typ, data: data, baseOffset: baseOffset, baseOid: baseOid, isDelta: isDelta, offset: start}
		order = append(order, start)
	}

	resolved := map[string]*GitObject{}
	byOffset := map[int]string{}
	record := func(start int, object *GitObject) {
		oid := hashObject(object.Type, object.Data)
		resolved[oid] = object
		byOffset[start] = oid
	}
	var resolveOffset func(start int, depth int) (*GitObject, error)
	resolveOffset = func(start int, depth int) (*GitObject, error) {
		if depth > 128 {
			return nil, errors.New("delta chain is too deep")
		}
		if known, ok := byOffset[start]; ok {
			return resolved[known], nil
		}
		entry, ok := entries[start]
		if !ok {
			return nil, errors.New("delta base offset is not in the pack")
		}
		if !entry.isDelta {
			object := &GitObject{Type: entry.typ, Data: entry.data}
			record(start, object)
			return object, nil
		}
		if entry.baseOffset != nil {
			base, err := resolveOffset(*entry.baseOffset, depth+1)
			if err != nil {
				return nil, err
			}
			data, err := applyDelta(base.Data, entry.data)
			if err != nil {
				return nil, err
			}
			object := &GitObject{Type: base.Type, Data: data}
			record(start, object)
			return object, nil
		}
		baseOid := *entry.baseOid
		base := resolved[baseOid]
		if base == nil && resolveExternal != nil {
			base = resolveExternal(baseOid)
		}
		if base == nil {
			return nil, fmt.Errorf("ref-delta base %s is not available", baseOid)
		}
		data, err := applyDelta(base.Data, entry.data)
		if err != nil {
			return nil, err
		}
		object := &GitObject{Type: base.Type, Data: data}
		record(start, object)
		return object, nil
	}

	for _, start := range order {
		if !entries[start].isDelta {
			if _, err := resolveOffset(start, 0); err != nil {
				return nil, err
			}
		}
	}
	for _, start := range order {
		e := entries[start]
		if e.isDelta && e.baseOffset != nil {
			if _, err := resolveOffset(start, 0); err != nil {
				return nil, err
			}
		}
	}
	pending := []int{}
	for _, start := range order {
		e := entries[start]
		if e.isDelta && e.baseOid != nil {
			if _, ok := byOffset[start]; !ok {
				pending = append(pending, start)
			}
		}
	}
	for len(pending) > 0 {
		before := len(pending)
		for _, start := range pending {
			e := entries[start]
			_, has := resolved[*e.baseOid]
			var ext *GitObject
			if resolveExternal != nil {
				ext = resolveExternal(*e.baseOid)
			}
			if has || ext != nil {
				if _, err := resolveOffset(start, 0); err != nil {
					return nil, err
				}
			}
		}
		next := []int{}
		for _, start := range pending {
			if _, ok := byOffset[start]; !ok {
				next = append(next, start)
			}
		}
		pending = next
		if len(pending) == before {
			if _, err := resolveOffset(pending[0], 0); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("ref-delta base %s is not available", *entries[pending[0]].baseOid)
		}
	}
	return resolved, nil
}

// --- Graph walks ---

func closureOf(repo *Repo, tips []string, boundary map[string]bool) map[string]bool {
	seen := map[string]bool{}
	stack := append([]string{}, tips...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		object := repo.Objects[oid]
		if object == nil {
			continue
		}
		seen[oid] = true
		switch object.Type {
		case "commit":
			view := parseCommit(object.Data)
			stack = append(stack, view.Tree)
			if boundary == nil || !boundary[oid] {
				stack = append(stack, view.Parents...)
			}
		case "tree":
			for _, e := range mustParseTree(object.Data) {
				stack = append(stack, e.Oid)
			}
		case "tag":
			stack = append(stack, parseTag(object.Data).Object)
		}
	}
	return seen
}

func peelToCommit(repo *Repo, oid string) *string {
	current := oid
	for hops := 0; hops < 16; hops++ {
		object := repo.Objects[current]
		if object == nil {
			return nil
		}
		if object.Type == "commit" {
			return &current
		}
		if object.Type == "tag" {
			current = parseTag(object.Data).Object
			continue
		}
		return nil
	}
	return nil
}

func isAncestor(repo *Repo, ancestor, descendant string) bool {
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == ancestor {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object := repo.Objects[oid]
		if object == nil || object.Type != "commit" {
			continue
		}
		stack = append(stack, parseCommit(object.Data).Parents...)
	}
	return false
}

func ancestorDistances(repo *Repo, oid string) (map[string]int, []string) {
	distances := map[string]int{}
	order := []string{}
	frontier := []string{oid}
	distance := 0
	for len(frontier) > 0 {
		next := []string{}
		for _, current := range frontier {
			if _, ok := distances[current]; ok {
				continue
			}
			distances[current] = distance
			order = append(order, current)
			object := repo.Objects[current]
			if object != nil && object.Type == "commit" {
				next = append(next, parseCommit(object.Data).Parents...)
			}
		}
		frontier = next
		distance++
	}
	return distances, order
}

func mergeBase(repo *Repo, a, b string) *string {
	if a == b {
		return &a
	}
	fromA, orderA := ancestorDistances(repo, a)
	fromB, _ := ancestorDistances(repo, b)
	var best *string
	bestScore := int(^uint(0) >> 1)
	for _, oid := range orderA {
		db, ok := fromB[oid]
		if !ok {
			continue
		}
		score := fromA[oid] + db
		if score < bestScore {
			bestScore = score
			v := oid
			best = &v
		}
	}
	return best
}

func historyOf(repo *Repo, tip string, cap int) []string {
	out := []string{}
	seen := map[string]bool{}
	frontier := []string{tip}
	for len(frontier) > 0 && len(out) < cap {
		next := []string{}
		for _, oid := range frontier {
			if seen[oid] {
				continue
			}
			seen[oid] = true
			out = append(out, oid)
			object := repo.Objects[oid]
			if object != nil && object.Type == "commit" {
				next = append(next, parseCommit(object.Data).Parents...)
			}
		}
		frontier = next
	}
	return out
}

// --- Trees as file maps, diffs, merges ---

type FileEntry struct {
	Oid  string
	Mode string
}

func flattenTree(repo *Repo, treeOid string, prefix string, out map[string]FileEntry) map[string]FileEntry {
	if out == nil {
		out = map[string]FileEntry{}
	}
	object := repo.Objects[treeOid]
	if object == nil || object.Type != "tree" {
		return out
	}
	for _, e := range mustParseTree(object.Data) {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			flattenTree(repo, e.Oid, path, out)
		} else {
			out[path] = FileEntry{Oid: e.Oid, Mode: e.Mode}
		}
	}
	return out
}

func treeOfCommit(repo *Repo, commitOid string) *string {
	object := repo.Objects[commitOid]
	if object == nil || object.Type != "commit" {
		return nil
	}
	tree := parseCommit(object.Data).Tree
	return &tree
}

func filesOfCommit(repo *Repo, commitOid string) map[string]FileEntry {
	tree := treeOfCommit(repo, commitOid)
	if tree == nil {
		return map[string]FileEntry{}
	}
	return flattenTree(repo, *tree, "", nil)
}

type FileChange struct {
	Path   string
	Status string // added | modified | removed
	Before *FileEntry
	After  *FileEntry
}

func diffCommits(repo *Repo, beforeOid, afterOid *string) []FileChange {
	var before, after map[string]FileEntry
	if beforeOid == nil {
		before = map[string]FileEntry{}
	} else {
		before = filesOfCommit(repo, *beforeOid)
	}
	if afterOid == nil {
		after = map[string]FileEntry{}
	} else {
		after = filesOfCommit(repo, *afterOid)
	}
	pathSet := map[string]bool{}
	for p := range before {
		pathSet[p] = true
	}
	for p := range after {
		pathSet[p] = true
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	changes := []FileChange{}
	for _, path := range paths {
		b, hasB := before[path]
		a, hasA := after[path]
		var bp, ap *FileEntry
		if hasB {
			v := b
			bp = &v
		}
		if hasA {
			v := a
			ap = &v
		}
		if bp != nil && ap != nil && bp.Oid == ap.Oid && bp.Mode == ap.Mode {
			continue
		}
		status := "modified"
		if bp == nil {
			status = "added"
		} else if ap == nil {
			status = "removed"
		}
		changes = append(changes, FileChange{Path: path, Status: status, Before: bp, After: ap})
	}
	return changes
}

func treeWithPath(repo *Repo, treeOid *string, segments []string, blob *FileEntry) string {
	var entries []TreeEntry
	if treeOid != nil {
		entries = mustParseTree(repo.Objects[*treeOid].Data)
	}
	head := segments[0]
	rest := segments[1:]
	kept := []TreeEntry{}
	for _, e := range entries {
		if e.Name != head {
			kept = append(kept, e)
		}
	}
	if len(rest) == 0 {
		if blob != nil {
			kept = append(kept, TreeEntry{Mode: blob.Mode, Name: head, Oid: blob.Oid})
		}
	} else {
		var existing *TreeEntry
		for i := range entries {
			if entries[i].Name == head && entries[i].Mode == "40000" {
				v := entries[i]
				existing = &v
			}
		}
		var existingOid *string
		if existing != nil {
			existingOid = &existing.Oid
		}
		childOid := treeWithPath(repo, existingOid, rest, blob)
		child := repo.Objects[childOid]
		if child != nil && len(mustParseTree(child.Data)) > 0 {
			kept = append(kept, TreeEntry{Mode: "40000", Name: head, Oid: childOid})
		}
	}
	return putObject(repo, "tree", buildTree(kept))
}

type mergeResult struct {
	Tree     *string
	Conflict *string
}

func mergeCommits(repo *Repo, baseOid *string, oursOid, theirsOid string) mergeResult {
	var baseTree *string
	if baseOid != nil {
		baseTree = treeOfCommit(repo, *baseOid)
	}
	oursTree := treeOfCommit(repo, oursOid)
	theirsTree := treeOfCommit(repo, theirsOid)
	if oursTree == nil || theirsTree == nil {
		c := ""
		return mergeResult{Conflict: &c}
	}
	return mergeTrees(repo, baseTree, *oursTree, *theirsTree, "")
}

func mergeTrees(repo *Repo, base *string, ours, theirs, prefix string) mergeResult {
	if ours == theirs {
		return mergeResult{Tree: &ours}
	}
	if base != nil && *base == ours {
		return mergeResult{Tree: &theirs}
	}
	if base != nil && *base == theirs {
		return mergeResult{Tree: &ours}
	}
	entryMap := func(oid *string) (map[string]TreeEntry, []string) {
		m := map[string]TreeEntry{}
		order := []string{}
		if oid == nil {
			return m, order
		}
		for _, e := range mustParseTree(repo.Objects[*oid].Data) {
			m[e.Name] = e
			order = append(order, e.Name)
		}
		return m, order
	}
	baseEntries, baseOrder := entryMap(base)
	var oursBase *string
	oursEntries, oursOrder := entryMap(&ours)
	theirsEntries, theirsOrder := entryMap(&theirs)
	_ = oursBase
	names := []string{}
	seenNames := map[string]bool{}
	for _, n := range append(append(append([]string{}, baseOrder...), oursOrder...), theirsOrder...) {
		if !seenNames[n] {
			seenNames[n] = true
			names = append(names, n)
		}
	}
	same := func(x, y *TreeEntry) bool {
		if x == nil && y == nil {
			return true
		}
		return x != nil && y != nil && x.Oid == y.Oid && x.Mode == y.Mode
	}
	out := []TreeEntry{}
	for _, name := range names {
		bv, hasB := baseEntries[name]
		ov, hasO := oursEntries[name]
		tv, hasT := theirsEntries[name]
		var b, o, t *TreeEntry
		if hasB {
			v := bv
			b = &v
		}
		if hasO {
			v := ov
			o = &v
		}
		if hasT {
			v := tv
			t = &v
		}
		if same(o, t) {
			if o != nil {
				out = append(out, *o)
			}
			continue
		}
		if same(b, o) {
			if t != nil {
				out = append(out, *t)
			}
			continue
		}
		if same(b, t) {
			if o != nil {
				out = append(out, *o)
			}
			continue
		}
		if o != nil && t != nil && o.Mode == "40000" && t.Mode == "40000" {
			var subBase *string
			if b != nil && b.Mode == "40000" {
				subBase = &b.Oid
			}
			sub := mergeTrees(repo, subBase, o.Oid, t.Oid, prefix+name+"/")
			if sub.Conflict != nil {
				return sub
			}
			out = append(out, TreeEntry{Mode: "40000", Name: name, Oid: *sub.Tree})
			continue
		}
		c := prefix + name
		return mergeResult{Conflict: &c}
	}
	tree := putObject(repo, "tree", buildTree(out))
	return mergeResult{Tree: &tree}
}

// --- Refs ---

type RefListing struct {
	Name         string
	Oid          string
	SymrefTarget *string
	Peeled       *string
}

func listRefs(repo *Repo) []RefListing {
	out := []RefListing{}
	headRef := "refs/heads/" + repo.DefaultBranch
	headOid, ok := repo.Refs[headRef]
	if !ok {
		headOid = zeroOID
	}
	out = append(out, RefListing{Name: "HEAD", Oid: headOid, SymrefTarget: &headRef})
	names := append([]string{}, repo.RefList...)
	sort.Strings(names)
	for _, name := range names {
		oid := repo.Refs[name]
		entry := RefListing{Name: name, Oid: oid}
		if object := repo.Objects[oid]; object != nil && object.Type == "tag" {
			peeled := parseTag(object.Data).Object
			entry.Peeled = &peeled
		}
		out = append(out, entry)
	}
	return out
}

// --- Fetch set computation ---

type FetchPlan struct {
	Objects   []*GitObject
	Shallow   []string
	Unshallow []string
	Acks      []string
	Nak       bool
}

func treeClosureOf(repo *Repo, treeOid string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{treeOid}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		object := repo.Objects[oid]
		if object == nil {
			continue
		}
		seen[oid] = true
		if object.Type == "tree" {
			for _, e := range mustParseTree(object.Data) {
				stack = append(stack, e.Oid)
			}
		}
	}
	return seen
}

func planFetch(repo *Repo, wants, haves, clientShallow []string, depth *int) FetchPlan {
	shallowSet := map[string]bool{}
	for _, oid := range clientShallow {
		shallowSet[oid] = true
	}
	havesPresent := []string{}
	for _, oid := range haves {
		if repo.Objects[oid] != nil {
			havesPresent = append(havesPresent, oid)
		}
	}
	var boundary map[string]bool
	if depth != nil {
		boundary = shallowSet
	}
	haveSet := closureOf(repo, havesPresent, boundary)
	haveCommits := map[string]bool{}
	for oid := range haveSet {
		if object := repo.Objects[oid]; object != nil && object.Type == "commit" {
			haveCommits[oid] = true
		}
	}
	send := map[string]bool{}
	acks := []string{}
	for _, have := range haves {
		if repo.Objects[have] != nil {
			acks = append(acks, have)
		}
	}

	wantCommits := []string{}
	for _, want := range wants {
		current := want
		for hops := 0; hops < 16; hops++ {
			object := repo.Objects[current]
			if object == nil || object.Type != "tag" {
				break
			}
			if !haveSet[current] {
				send[current] = true
			}
			current = parseTag(object.Data).Object
		}
		if commit := peelToCommit(repo, want); commit != nil {
			wantCommits = append(wantCommits, *commit)
		}
	}

	includedCommits := map[string]bool{}
	if depth == nil {
		for oid := range closureOf(repo, wants, nil) {
			if !haveSet[oid] {
				send[oid] = true
			}
		}
		for oid := range send {
			if object := repo.Objects[oid]; object != nil && object.Type == "commit" {
				includedCommits[oid] = true
			}
		}
	} else {
		distance := map[string]int{}
		distanceOrder := []string{}
		type qItem struct {
			oid string
			d   int
		}
		queue := []qItem{}
		for _, commit := range wantCommits {
			queue = append(queue, qItem{commit, 0})
		}
		for head := 0; head < len(queue); head++ {
			item := queue[head]
			if item.d >= *depth {
				continue
			}
			if existing, ok := distance[item.oid]; ok && existing <= item.d {
				continue
			}
			if _, ok := distance[item.oid]; !ok {
				distanceOrder = append(distanceOrder, item.oid)
			}
			distance[item.oid] = item.d
			object := repo.Objects[item.oid]
			if object == nil || object.Type != "commit" {
				continue
			}
			for _, parent := range parseCommit(object.Data).Parents {
				queue = append(queue, qItem{parent, item.d + 1})
			}
		}
		for _, oid := range distanceOrder {
			if !haveSet[oid] {
				includedCommits[oid] = true
			}
		}
		for oid := range includedCommits {
			send[oid] = true
			view := parseCommit(repo.Objects[oid].Data)
			for treeOid := range treeClosureOf(repo, view.Tree) {
				if !haveSet[treeOid] {
					send[treeOid] = true
				}
			}
		}
	}

	shallow := []string{}
	unshallow := []string{}
	if depth != nil {
		present := map[string]bool{}
		for oid := range includedCommits {
			present[oid] = true
		}
		for oid := range haveCommits {
			if !shallowSet[oid] {
				present[oid] = true
			}
		}
		candidateSet := map[string]bool{}
		candidates := []string{}
		for oid := range includedCommits {
			if !candidateSet[oid] {
				candidateSet[oid] = true
				candidates = append(candidates, oid)
			}
		}
		for _, oid := range clientShallow {
			if !candidateSet[oid] {
				candidateSet[oid] = true
				candidates = append(candidates, oid)
			}
		}
		for _, oid := range candidates {
			object := repo.Objects[oid]
			if object == nil || object.Type != "commit" {
				continue
			}
			parents := parseCommit(object.Data).Parents
			if len(parents) == 0 {
				continue
			}
			cut := false
			for _, parent := range parents {
				if !present[parent] && repo.Objects[parent] != nil {
					cut = true
					break
				}
			}
			if cut {
				shallow = append(shallow, oid)
			} else if shallowSet[oid] {
				unshallow = append(unshallow, oid)
			}
		}
	}

	byType := map[string][]string{"commit": {}, "tree": {}, "blob": {}, "tag": {}}
	for oid := range send {
		if object := repo.Objects[oid]; object != nil {
			byType[object.Type] = append(byType[object.Type], oid)
		}
	}
	objects := []*GitObject{}
	for _, typ := range []string{"commit", "tree", "blob", "tag"} {
		oids := byType[typ]
		sort.Strings(oids)
		for _, oid := range oids {
			objects = append(objects, repo.Objects[oid])
		}
	}
	sort.Strings(shallow)
	sort.Strings(unshallow)
	return FetchPlan{Objects: objects, Shallow: shallow, Unshallow: unshallow, Acks: acks, Nak: len(acks) == 0}
}

var _ = strings.TrimSpace
