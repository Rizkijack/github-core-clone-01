package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const maxChunk = 65515

func pkt(payload []byte) []byte {
	length := len(payload) + 4
	hex := strconv.FormatInt(int64(length), 16)
	for len(hex) < 4 {
		hex = "0" + hex
	}
	out := make([]byte, 0, length)
	out = append(out, []byte(hex)...)
	out = append(out, payload...)
	return out
}
func pktStr(s string) []byte { return pkt([]byte(s)) }

var flushPkt = []byte("0000")

type packet struct {
	kind    byte // 'd' data, 'f' flush, 'm' delim, 'e' end
	payload []byte
}

func decodePkts(buf []byte) ([]packet, []byte) {
	var packets []packet
	i := 0
	for i+4 <= len(buf) {
		hex := string(buf[i : i+4])
		if !isHex4(hex) {
			break
		}
		length, _ := strconv.ParseUint(hex, 16, 32)
		if length == 0 {
			packets = append(packets, packet{kind: 'f'})
			i += 4
			continue
		}
		if length == 1 {
			packets = append(packets, packet{kind: 'm'})
			i += 4
			continue
		}
		if length == 2 {
			packets = append(packets, packet{kind: 'e'})
			i += 4
			continue
		}
		if length < 4 || i+int(length) > len(buf) {
			break
		}
		packets = append(packets, packet{kind: 'd', payload: buf[i+4 : i+int(length)]})
		i += int(length)
	}
	return packets, buf[i:]
}

func isHex4(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return len(s) == 4
}

func gitRes(w http.ResponseWriter, service, kind string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/x-"+service+"-"+kind)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	w.Write(body)
}

// ---- pack codec ----
var typeCode = map[string]byte{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[byte]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func packHeader(typ string, size int) []byte {
	code := typeCode[typ]
	var out []byte
	remaining := size
	first := (code << 4) | byte(remaining&0x0f)
	remaining >>= 4
	for remaining > 0 {
		out = append(out, first|0x80)
		first = byte(remaining & 0x7f)
		remaining >>= 7
	}
	out = append(out, first)
	return out
}

func deflateBytes(data []byte) []byte {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write(data)
	zw.Close()
	return buf.Bytes()
}

func buildPack(objects []*GitObject) []byte {
	var body bytes.Buffer
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	binary.BigEndian.PutUint32(header[4:8], 2)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(objects)))
	body.Write(header)
	for _, o := range objects {
		body.Write(packHeader(o.Type, len(o.Data)))
		body.Write(deflateBytes(o.Data))
	}
	b := body.Bytes()
	sum := sha1.Sum(b)
	return append(b, sum[:]...)
}

func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() int {
		value, shift := 0, uint(0)
		for {
			b := delta[off]
			off++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value
	}
	sourceSize := readSize()
	targetSize := readSize()
	if sourceSize != len(base) {
		return nil, errString("delta base size mismatch")
	}
	out := make([]byte, targetSize)
	written := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			copyOffset, copySize := 0, 0
			shift := uint(0)
			for bit := uint(0); bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					copyOffset |= int(delta[off]) << shift
					off++
					shift += 8
				}
			}
			shift = 0
			for bit := uint(4); bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					copySize |= int(delta[off]) << shift
					off++
					shift += 8
				}
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			copy(out[written:written+copySize], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			n := int(op)
			copy(out[written:written+n], delta[off:off+n])
			off += n
			written += n
		} else {
			return nil, errString("delta opcode 0")
		}
	}
	return out, nil
}

type errString string

func (e errString) Error() string { return string(e) }

func inflateAt(buf []byte, off, limit int) ([]byte, int, error) {
	src := bytes.NewReader(buf[off:limit])
	startLen := src.Len()
	zr, err := zlib.NewReader(src)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	return data, startLen - src.Len(), nil
}

type rawEntry struct {
	offset     int
	typeCode   byte
	size       int
	data       []byte
	baseOffset int
	baseOid    string
	resolved   []byte
	rtype      string
	oid        string
	done       bool
}

func parsePack(buf []byte) (map[string]*GitObject, error) {
	if len(buf) < 32 || string(buf[0:4]) != "PACK" {
		return nil, errString("bad pack header")
	}
	version := binary.BigEndian.Uint32(buf[4:8])
	if version != 2 && version != 3 {
		return nil, errString("bad pack version")
	}
	count := int(binary.BigEndian.Uint32(buf[8:12]))
	trailerStart := len(buf) - 20
	sum := sha1.Sum(buf[:trailerStart])
	if hexEncode(sum[:]) != hexEncode(buf[trailerStart:]) {
		return nil, errString("pack checksum mismatch")
	}
	var raw []*rawEntry
	byOffset := map[int]*rawEntry{}
	off := 12
	for n := 0; n < count; n++ {
		entryOffset := off
		b := buf[off]
		first := b
		size := int(b & 0x0f)
		shift := uint(4)
		off++
		for b&0x80 != 0 {
			b = buf[off]
			off++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		tc := (first >> 4) & 0x07
		entry := &rawEntry{offset: entryOffset, typeCode: tc, size: size}
		if tc == 6 {
			bb := buf[off]
			off++
			value := int(bb & 0x7f)
			for bb&0x80 != 0 {
				bb = buf[off]
				off++
				value = (value+1)*128 + int(bb&0x7f)
			}
			entry.baseOffset = entryOffset - value
		} else if tc == 7 {
			entry.baseOid = hexEncode(buf[off : off+20])
			off += 20
		} else if _, ok := codeType[tc]; !ok {
			return nil, errString("bad pack type")
		}
		data, consumed, err := inflateAt(buf, off, trailerStart)
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, errString("pack size mismatch")
		}
		entry.data = data
		off += consumed
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	objects := map[string]*GitObject{}
	byOid := map[string]*rawEntry{}
	var resolve func(e *rawEntry) (string, []byte, string, error)
	resolve = func(e *rawEntry) (string, []byte, string, error) {
		if e.done {
			return e.rtype, e.resolved, e.oid, nil
		}
		var typ string
		var data []byte
		if e.typeCode == 6 {
			base := byOffset[e.baseOffset]
			bt, bd, _, err := resolve(base)
			if err != nil {
				return "", nil, "", err
			}
			typ = bt
			data, err = applyDelta(bd, e.data)
			if err != nil {
				return "", nil, "", err
			}
		} else if e.typeCode == 7 {
			baseEntry := byOid[e.baseOid]
			if baseEntry == nil {
				return "", nil, "", errString("ref-delta base missing")
			}
			bt, bd, _, err := resolve(baseEntry)
			if err != nil {
				return "", nil, "", err
			}
			typ = bt
			data, err = applyDelta(bd, e.data)
			if err != nil {
				return "", nil, "", err
			}
		} else {
			typ = codeType[e.typeCode]
			data = e.data
		}
		oid := gitOid(typ, data)
		e.resolved = data
		e.rtype = typ
		e.oid = oid
		e.done = true
		byOid[oid] = e
		return typ, data, oid, nil
	}
	for _, e := range raw {
		typ, data, oid, err := resolve(e)
		if err != nil {
			return nil, err
		}
		if _, ok := objects[oid]; !ok {
			objects[oid] = &GitObject{Type: typ, Data: data}
		}
	}
	return objects, nil
}

// ---- reachability ----
func parseTagObj(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "object ") {
			oid := strings.TrimSpace(line[7:])
			if isSha40(oid) {
				return strings.ToLower(oid)
			}
		}
	}
	return ""
}
func peel(s *Store, r *Repo, oid string) string {
	cur := oid
	for i := 0; i < 16; i++ {
		obj := s.getObject(r, cur)
		if obj == nil || obj.Type != "tag" {
			return cur
		}
		cur = parseTagObj(obj.Data)
	}
	return cur
}
func addClosure(s *Store, r *Repo, oid string, out map[string]bool) {
	if out[oid] {
		return
	}
	obj := s.getObject(r, oid)
	if obj == nil {
		return
	}
	out[oid] = true
	if obj.Type == "tree" {
		for _, e := range parseTree(obj.Data) {
			addClosure(s, r, e.Oid, out)
		}
	} else if obj.Type == "tag" {
		addClosure(s, r, parseTagObj(obj.Data), out)
	}
}

type reachOpts struct {
	maxDepth int
	cut      map[string]bool
	boundary map[string]bool
}

func reach(s *Store, r *Repo, roots []string, opts reachOpts) map[string]bool {
	maxDepth := opts.maxDepth
	if maxDepth == 0 {
		maxDepth = int(^uint(0) >> 1)
	}
	cut := opts.cut
	if cut == nil {
		cut = map[string]bool{}
	}
	out := map[string]bool{}
	commitDepth := map[string]int{}
	type item struct {
		oid   string
		depth int
	}
	stack := make([]item, 0, len(roots))
	for _, o := range roots {
		stack = append(stack, item{o, 1})
	}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		obj := s.getObject(r, it.oid)
		if obj == nil {
			continue
		}
		if obj.Type == "commit" {
			if prev, ok := commitDepth[it.oid]; ok && prev <= it.depth {
				continue
			}
			commitDepth[it.oid] = it.depth
			out[it.oid] = true
			ci := parseCommit(obj.Data)
			addClosure(s, r, ci.Tree, out)
			atBoundary := it.depth >= maxDepth
			if atBoundary && opts.boundary != nil {
				opts.boundary[it.oid] = true
			}
			if !cut[it.oid] && !atBoundary {
				for _, p := range ci.Parents {
					stack = append(stack, item{p, it.depth + 1})
				}
			}
		} else {
			addClosure(s, r, it.oid, out)
		}
	}
	return out
}

// ---- advertisement ----
func advertise(s *Store, r *Repo, service string, v2 bool) []byte {
	if v2 && service == "git-upload-pack" {
		var b bytes.Buffer
		b.Write(pktStr("version 2\n"))
		b.Write(pktStr("ls-refs=unborn\n"))
		b.Write(pktStr("fetch=shallow\n"))
		b.Write(pktStr("object-format=sha1\n"))
		b.Write(pktStr("agent=forge/1.0\n"))
		b.Write(flushPkt)
		return b.Bytes()
	}
	caps := "multi_ack_detailed side-band-64k thin-pack ofs-delta shallow no-progress include-tag symref=HEAD:refs/heads/main agent=forge/1.0"
	if service == "git-receive-pack" {
		caps = "report-status side-band-64k delete-refs ofs-delta symref=HEAD:refs/heads/main agent=forge/1.0"
	}
	var b bytes.Buffer
	b.Write(pktStr("# service=" + service + "\n"))
	b.Write(flushPkt)
	headTip := s.resolveRef(r, "HEAD")
	var lines []string
	if headTip != "" {
		lines = append(lines, headTip+" HEAD")
	}
	for ref, oid := range r.Refs {
		lines = append(lines, oid+" "+ref)
	}
	if len(lines) == 0 {
		b.Write(pktStr(zeroOID + " capabilities^{}\x00" + caps + "\n"))
	} else {
		for i, line := range lines {
			if i == 0 {
				b.Write(pktStr(line + "\x00" + caps + "\n"))
			} else {
				b.Write(pktStr(line + "\n"))
			}
		}
		for ref, oid := range r.Refs {
			if strings.HasPrefix(ref, "refs/tags/") {
				obj := s.getObject(r, oid)
				if obj != nil && obj.Type == "tag" {
					b.Write(pktStr(peel(s, r, oid) + " " + ref + "^{}\n"))
				}
			}
		}
	}
	b.Write(flushPkt)
	return b.Bytes()
}

func lsRefs(s *Store, r *Repo, packets []packet) []byte {
	peelFlag, symrefs, unborn := false, false, false
	var prefixes []string
	afterDelim := false
	for _, p := range packets {
		if p.kind == 'm' {
			afterDelim = true
			continue
		}
		if p.kind != 'd' || !afterDelim {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if line == "peel" {
			peelFlag = true
		} else if line == "symrefs" {
			symrefs = true
		} else if line == "unborn" {
			unborn = true
		} else if strings.HasPrefix(line, "ref-prefix ") {
			prefixes = append(prefixes, line[11:])
		}
	}
	match := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var b bytes.Buffer
	headTip := s.resolveRef(r, "HEAD")
	if symrefs && match("HEAD") {
		if headTip != "" {
			b.Write(pktStr(headTip + " HEAD symref-target:" + r.Head + "\n"))
		} else if unborn && len(r.Refs) == 0 {
			b.Write(pktStr("unborn HEAD symref-target:" + r.Head + "\n"))
		}
	}
	for ref, oid := range r.Refs {
		if !match(ref) {
			continue
		}
		obj := s.getObject(r, oid)
		if peelFlag && obj != nil && obj.Type == "tag" {
			b.Write(pktStr(oid + " " + ref + " peeled:" + peel(s, r, oid) + "\n"))
		} else {
			b.Write(pktStr(oid + " " + ref + "\n"))
		}
	}
	b.Write(flushPkt)
	return b.Bytes()
}

func sidebandPack(pack []byte) [][]byte {
	var parts [][]byte
	if len(pack) == 0 {
		parts = append(parts, pkt(append([]byte{1}, pack...)))
		return parts
	}
	for i := 0; i < len(pack); i += maxChunk {
		end := i + maxChunk
		if end > len(pack) {
			end = len(pack)
		}
		chunk := append([]byte{1}, pack[i:end]...)
		parts = append(parts, pkt(chunk))
	}
	return parts
}

func doFetch(s *Store, r *Repo, packets []packet, v2 bool) []byte {
	var wants, haves, clientShallow []string
	deepen := -1
	for _, p := range packets {
		if p.kind != 'd' {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if strings.HasPrefix(line, "want ") {
			wants = append(wants, strings.Split(line[5:], " ")[0])
		} else if strings.HasPrefix(line, "have ") {
			haves = append(haves, strings.TrimSpace(line[5:]))
		} else if strings.HasPrefix(line, "shallow ") {
			clientShallow = append(clientShallow, strings.TrimSpace(line[8:]))
		} else if strings.HasPrefix(line, "deepen ") {
			deepen, _ = strconv.Atoi(strings.TrimSpace(line[7:]))
		}
	}
	cut := map[string]bool{}
	for _, c := range clientShallow {
		cut[c] = true
	}
	clientHasRoots := append(append([]string{}, haves...), clientShallow...)
	clientHas := reach(s, r, clientHasRoots, reachOpts{cut: cut})
	boundary := map[string]bool{}
	maxDepth := 0
	if deepen >= 0 {
		maxDepth = deepen
	}
	wanted := reach(s, r, wants, reachOpts{maxDepth: maxDepth, boundary: boundary})
	var sendObjects []*GitObject
	for o := range wanted {
		if !clientHas[o] {
			if obj := s.getObject(r, o); obj != nil {
				sendObjects = append(sendObjects, obj)
			}
		}
	}
	pack := buildPack(sendObjects)
	var b bytes.Buffer
	if v2 {
		b.Write(pktStr("acknowledgments\n"))
		b.Write(pktStr("NAK\n"))
		b.Write(flushPkt)
		if deepen >= 0 {
			b.Write(pktStr("shallow-info\n"))
			for bb := range boundary {
				b.Write(pktStr("shallow " + bb + "\n"))
			}
			for _, c := range clientShallow {
				if !boundary[c] && wanted[c] {
					b.Write(pktStr("unshallow " + c + "\n"))
				}
			}
			b.Write(flushPkt)
		}
		b.Write(pktStr("packfile\n"))
		for _, p := range sidebandPack(pack) {
			b.Write(p)
		}
		b.Write(flushPkt)
	} else {
		if deepen >= 0 {
			for bb := range boundary {
				b.Write(pktStr("shallow " + bb + "\n"))
			}
			for _, c := range clientShallow {
				if !boundary[c] && wanted[c] {
					b.Write(pktStr("unshallow " + c + "\n"))
				}
			}
		}
		b.Write(pktStr("NAK\n"))
		for _, p := range sidebandPack(pack) {
			b.Write(p)
		}
	}
	return b.Bytes()
}

func doReceive(s *Store, r *Repo, body []byte) []byte {
	packets, rest := decodePkts(body)
	type cmd struct{ old, new, ref string }
	var commands []cmd
	caps := ""
	for _, p := range packets {
		if p.kind != 'd' {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			caps = line[nul+1:]
			line = line[:nul]
		}
		fields := strings.SplitN(line, " ", 3)
		if len(fields) == 3 && isSha40(fields[0]) && isSha40(fields[1]) {
			commands = append(commands, cmd{strings.ToLower(fields[0]), strings.ToLower(fields[1]), fields[2]})
		}
	}
	sideband := strings.Contains(caps, "side-band-64k")
	unpackOk := true
	if len(rest) > 0 {
		objs, err := parsePack(rest)
		if err != nil {
			unpackOk = false
		} else {
			for oid, o := range objs {
				if _, ok := r.Objects[oid]; !ok {
					r.Objects[oid] = o
				}
			}
		}
	}
	var report []string
	if unpackOk {
		report = append(report, "unpack ok")
	} else {
		report = append(report, "unpack error")
	}
	if unpackOk {
		for _, c := range commands {
			current := r.Refs[c.ref]
			if current == "" {
				current = zeroOID
			}
			ok := false
			if c.new == zeroOID {
				ok = current == c.old
				if _, exists := r.Refs[c.ref]; !exists {
					ok = false
				}
				if ok {
					delete(r.Refs, c.ref)
				}
			} else if c.old == zeroOID {
				_, exists := r.Refs[c.ref]
				_, hasObj := r.Objects[c.new]
				ok = !exists && hasObj
				if ok {
					r.Refs[c.ref] = c.new
				}
			} else {
				_, hasObj := r.Objects[c.new]
				ff := current == c.old && hasObj && s.isAncestor(r, c.new, c.old)
				ok = ff
				if ok {
					r.Refs[c.ref] = c.new
				}
			}
			if ok {
				report = append(report, "ok "+c.ref)
			} else {
				report = append(report, "ng "+c.ref+" non-fast-forward")
			}
		}
	}
	var inner bytes.Buffer
	for _, l := range report {
		inner.Write(pktStr(l + "\n"))
	}
	inner.Write(flushPkt)
	innerBytes := inner.Bytes()
	if sideband {
		var b bytes.Buffer
		for i := 0; i < len(innerBytes); i += maxChunk {
			end := i + maxChunk
			if end > len(innerBytes) {
				end = len(innerBytes)
			}
			b.Write(pkt(append([]byte{1}, innerBytes[i:end]...)))
		}
		return b.Bytes()
	}
	return innerBytes
}

func handleGit(s *Store, ctx *Ctx, w http.ResponseWriter, repo *Repo, gitPath string) {
	if !ctx.Authed {
		unauth(w)
		return
	}
	service := ctx.Query.Get("service")
	v2 := ctx.Header.Get("Git-Protocol") == "version=2"
	if gitPath == "/info/refs" && ctx.Method == "GET" {
		if service != "git-upload-pack" && service != "git-receive-pack" {
			badReq(w, "bad service")
			return
		}
		useV2 := v2 && service == "git-upload-pack"
		gitRes(w, service, "advertisement", advertise(s, repo, service, useV2))
		return
	}
	if gitPath == "/git-upload-pack" && ctx.Method == "POST" {
		packets, _ := decodePkts(ctx.RawBody)
		command := ""
		for _, p := range packets {
			if p.kind == 'd' && strings.HasPrefix(string(p.payload), "command=") {
				command = strings.TrimSuffix(string(p.payload)[8:], "\n")
				break
			}
		}
		if v2 && command == "ls-refs" {
			gitRes(w, "git-upload-pack", "result", lsRefs(s, repo, packets))
			return
		}
		if v2 && command == "fetch" {
			gitRes(w, "git-upload-pack", "result", doFetch(s, repo, packets, true))
			return
		}
		if !v2 {
			gitRes(w, "git-upload-pack", "result", doFetch(s, repo, packets, false))
			return
		}
		badReq(w, "bad upload-pack request")
		return
	}
	if gitPath == "/git-receive-pack" && ctx.Method == "POST" {
		gitRes(w, "git-receive-pack", "result", doReceive(s, repo, ctx.RawBody))
		return
	}
	notFound(w)
}
