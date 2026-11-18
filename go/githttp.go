package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Git smart HTTP: a faithful port of the reference githttp.ts, including its
// fetch/advertisement memoization (keyed by a state fingerprint), which the
// clone-heavy bench depends on.

type HttpResult struct {
	Status      int
	ContentType string
	Body        []byte
	Headers     map[string]string
}

const uploadCapsV0 = "side-band-64k multi_ack_detailed thin-pack ofs-delta shallow"
const receiveCaps = "report-status delete-refs side-band-64k ofs-delta"
const agentString = "github-core-clone/1.0"

func textResult(status int, body string) HttpResult {
	return HttpResult{Status: status, ContentType: "text/plain; charset=utf-8", Body: []byte(body)}
}

func gitResult(service, kind string, body []byte) HttpResult {
	return HttpResult{
		Status:      200,
		ContentType: fmt.Sprintf("application/x-%s-%s", service, kind),
		Body:        body,
		Headers:     map[string]string{"cache-control": "no-cache, max-age=0, must-revalidate"},
	}
}

func sidebandPackets(band byte, data []byte) [][]byte {
	out := [][]byte{}
	const chunk = 65512
	if len(data) == 0 {
		return out
	}
	for at := 0; at < len(data); at += chunk {
		end := at + chunk
		if end > len(data) {
			end = len(data)
		}
		payload := append([]byte{band}, data[at:end]...)
		out = append(out, pktLineBytes(payload))
	}
	return out
}

type GitHttp struct {
	store     *Store
	fetchMemo map[string][]byte
	adsMemo   map[string][]byte
}

func NewGitHttp(store *Store) *GitHttp {
	return &GitHttp{store: store, fetchMemo: map[string][]byte{}, adsMemo: map[string][]byte{}}
}

func (g *GitHttp) memoizeAds(key string, body []byte) []byte {
	if len(g.adsMemo) >= 128 {
		g.adsMemo = map[string][]byte{}
	}
	g.adsMemo[key] = body
	return body
}

func (g *GitHttp) stateFingerprint(repo *Repo) string {
	var sb strings.Builder
	sb.WriteString(strconv.Itoa(len(repo.Objects)))
	sb.WriteByte('|')
	for _, name := range repo.RefList {
		sb.WriteString(name + "=" + repo.Refs[name] + ";")
	}
	return sb.String()
}

func (g *GitHttp) fetchKey(repo *Repo, variant string, wants, haves, shallow []string, depth *int) string {
	depthStr := ""
	if depth != nil {
		depthStr = strconv.Itoa(*depth)
	}
	return strings.Join([]string{
		variant,
		strconv.Itoa(repo.DatabaseID),
		g.stateFingerprint(repo),
		strings.Join(wants, ","),
		strings.Join(haves, ","),
		strings.Join(shallow, ","),
		depthStr,
	}, "|")
}

func (g *GitHttp) memoizeFetch(key string, body []byte) []byte {
	if len(g.fetchMemo) >= 128 {
		g.fetchMemo = map[string][]byte{}
	}
	g.fetchMemo[key] = body
	return body
}

func concatBytes(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

func (g *GitHttp) infoRefs(owner, name string, service *string, protocolV2 bool) HttpResult {
	svc := ""
	if service != nil {
		svc = *service
	}
	if svc != "git-upload-pack" && svc != "git-receive-pack" {
		return textResult(400, "unknown service\n")
	}
	repo := g.store.RepoOf(owner, name)
	if repo == nil {
		return textResult(404, "repository not found\n")
	}
	variant := "v0"
	if protocolV2 && svc == "git-upload-pack" {
		variant = "v2"
	}
	adsKey := fmt.Sprintf("%d|%s|%s|%s|%s", repo.DatabaseID, svc, variant, repo.DefaultBranch, g.stateFingerprint(repo))
	if memoized, ok := g.adsMemo[adsKey]; ok {
		return gitResult(svc, "advertisement", memoized)
	}
	if svc == "git-upload-pack" && protocolV2 {
		parts := [][]byte{
			pktLine("# service=git-upload-pack\n"),
			pktFlush,
			pktLine("version 2\n"),
			pktLine("agent=" + agentString + "\n"),
			pktLine("ls-refs=unborn\n"),
			pktLine("fetch=shallow wait-for-done\n"),
			pktLine("object-format=sha1\n"),
			pktFlush,
		}
		return gitResult("git-upload-pack", "advertisement", g.memoizeAds(adsKey, concatBytes(parts...)))
	}
	caps := ""
	if svc == "git-upload-pack" {
		caps = uploadCapsV0 + " agent=" + agentString
	} else {
		caps = receiveCaps + " agent=" + agentString
	}
	parts := [][]byte{pktLine("# service=" + svc + "\n"), pktFlush}
	refs := listRefs(repo)
	headRef := "refs/heads/" + repo.DefaultBranch
	if svc == "git-upload-pack" {
		lines := []string{}
		var head *RefListing
		for i := range refs {
			if refs[i].Name == "HEAD" {
				head = &refs[i]
			}
		}
		if len(repo.Refs) == 0 {
			lines = append(lines, zeroOID+" capabilities^{}")
		} else if head != nil {
			lines = append(lines, head.Oid+" HEAD")
			for _, ref := range refs {
				if ref.Name == "HEAD" {
					continue
				}
				lines = append(lines, ref.Oid+" "+ref.Name)
				if ref.Peeled != nil {
					lines = append(lines, *ref.Peeled+" "+ref.Name+"^{}")
				}
			}
		}
		for i, line := range lines {
			if i == 0 {
				line = line + "\x00" + caps + " symref=HEAD:" + headRef
			}
			parts = append(parts, pktLine(line+"\n"))
		}
	} else {
		real := []RefListing{}
		for _, ref := range refs {
			if ref.Name != "HEAD" {
				real = append(real, ref)
			}
		}
		if len(real) == 0 {
			parts = append(parts, pktLine(zeroOID+" capabilities^{}\x00"+caps+"\n"))
		} else {
			for i, ref := range real {
				suffix := ""
				if i == 0 {
					suffix = "\x00" + caps
				}
				parts = append(parts, pktLine(ref.Oid+" "+ref.Name+suffix+"\n"))
				if ref.Peeled != nil {
					parts = append(parts, pktLine(*ref.Peeled+" "+ref.Name+"^{}\n"))
				}
			}
		}
	}
	parts = append(parts, pktFlush)
	return gitResult(svc, "advertisement", g.memoizeAds(adsKey, concatBytes(parts...)))
}

func (g *GitHttp) uploadPack(owner, name string, body []byte) (result HttpResult) {
	repo := g.store.RepoOf(owner, name)
	if repo == nil {
		return textResult(404, "repository not found\n")
	}
	packets, err := parseAllPkts(body)
	if err != nil {
		return textResult(400, "malformed pkt-line request\n")
	}
	firstLine := ""
	for _, packet := range packets {
		if packet.kind == "data" {
			firstLine = string(packet.payload)
			break
		}
	}
	// The reference wraps the command handlers in a try/catch that answers 400
	// with the failure's message; a Go panic lands in the same place.
	defer func() {
		if r := recover(); r != nil {
			result = textResult(400, fmt.Sprintf("upload-pack failed: %v\n", r))
		}
	}()
	if strings.HasPrefix(firstLine, "command=ls-refs") {
		return g.lsRefs(repo, packets)
	}
	if strings.HasPrefix(firstLine, "command=fetch") {
		res, err := g.fetchV2(repo, packets)
		if err != nil {
			return textResult(400, "upload-pack failed: "+err.Error()+"\n")
		}
		return res
	}
	res, err := g.fetchV0(repo, packets)
	if err != nil {
		return textResult(400, "upload-pack failed: "+err.Error()+"\n")
	}
	return res
}

func trimOneNewline(s string) string {
	return strings.TrimSuffix(s, "\n")
}

func (g *GitHttp) lsRefs(repo *Repo, packets []pkt) HttpResult {
	peel := false
	symrefs := false
	unborn := false
	prefixes := []string{}
	afterDelim := false
	for _, packet := range packets {
		if packet.kind == "delim" {
			afterDelim = true
			continue
		}
		if !afterDelim || packet.kind != "data" {
			continue
		}
		line := trimOneNewline(string(packet.payload))
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, line[len("ref-prefix "):])
		}
	}
	peelBit, symBit, unbornBit := "0", "0", "0"
	if peel {
		peelBit = "1"
	}
	if symrefs {
		symBit = "1"
	}
	if unborn {
		unbornBit = "1"
	}
	adsKey := fmt.Sprintf("%d|ls-refs|%s%s%s|%s|%s|%s", repo.DatabaseID, peelBit, symBit, unbornBit, strings.Join(prefixes, ","), repo.DefaultBranch, g.stateFingerprint(repo))
	if memoized, ok := g.adsMemo[adsKey]; ok {
		return gitResult("git-upload-pack", "result", memoized)
	}
	parts := [][]byte{}
	for _, ref := range listRefs(repo) {
		if len(prefixes) > 0 {
			matched := false
			for _, prefix := range prefixes {
				if strings.HasPrefix(ref.Name, prefix) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		attrs := []string{}
		if symrefs && ref.SymrefTarget != nil {
			attrs = append(attrs, "symref-target:"+*ref.SymrefTarget)
		}
		if peel && ref.Peeled != nil {
			attrs = append(attrs, "peeled:"+*ref.Peeled)
		}
		attrText := ""
		if len(attrs) > 0 {
			attrText = " " + strings.Join(attrs, " ")
		}
		var line string
		if ref.Name == "HEAD" && ref.Oid == zeroOID && unborn {
			line = "unborn HEAD" + attrText
		} else {
			line = ref.Oid + " " + ref.Name + attrText
		}
		parts = append(parts, pktLine(line+"\n"))
	}
	parts = append(parts, pktFlush)
	return gitResult("git-upload-pack", "result", g.memoizeAds(adsKey, concatBytes(parts...)))
}

func (g *GitHttp) fetchV2(repo *Repo, packets []pkt) (HttpResult, error) {
	wants := []string{}
	haves := []string{}
	shallow := []string{}
	var depth *int
	afterDelim := false
	for _, packet := range packets {
		if packet.kind == "delim" {
			afterDelim = true
			continue
		}
		if !afterDelim || packet.kind != "data" {
			continue
		}
		line := trimOneNewline(string(packet.payload))
		switch {
		case strings.HasPrefix(line, "want "):
			wants = append(wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			shallow = append(shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			n, err := strconv.Atoi(strings.TrimSpace(line[7:]))
			if err != nil {
				n = 0
			}
			depth = &n
		}
	}
	for _, want := range wants {
		if repo.Objects[want] == nil {
			body := concatBytes(pktLine(fmt.Sprintf("ERR upload-pack: not our ref %s\n", want)), pktFlush)
			return gitResult("git-upload-pack", "result", body), nil
		}
	}
	memoKey := g.fetchKey(repo, "v2", wants, haves, shallow, depth)
	if memoized, ok := g.fetchMemo[memoKey]; ok {
		return gitResult("git-upload-pack", "result", memoized), nil
	}
	plan := planFetch(repo, wants, haves, shallow, depth)
	parts := [][]byte{}
	parts = append(parts, pktLine("acknowledgments\n"))
	if len(plan.Acks) == 0 {
		parts = append(parts, pktLine("NAK\n"))
	} else {
		for _, ack := range plan.Acks {
			parts = append(parts, pktLine("ACK "+ack+"\n"))
		}
	}
	if len(plan.Objects) > 0 {
		parts = append(parts, pktLine("ready\n"))
	}
	parts = append(parts, pktDelim)
	if len(plan.Shallow) > 0 || len(plan.Unshallow) > 0 {
		parts = append(parts, pktLine("shallow-info\n"))
		for _, oid := range plan.Shallow {
			parts = append(parts, pktLine("shallow "+oid+"\n"))
		}
		for _, oid := range plan.Unshallow {
			parts = append(parts, pktLine("unshallow "+oid+"\n"))
		}
		parts = append(parts, pktDelim)
	}
	if len(plan.Objects) > 0 {
		parts = append(parts, pktLine("packfile\n"))
		pack := writePack(plan.Objects)
		parts = append(parts, sidebandPackets(1, pack)...)
	}
	parts = append(parts, pktFlush)
	return gitResult("git-upload-pack", "result", g.memoizeFetch(memoKey, concatBytes(parts...))), nil
}

func (g *GitHttp) fetchV0(repo *Repo, packets []pkt) (HttpResult, error) {
	wants := []string{}
	haves := []string{}
	shallow := []string{}
	var depth *int
	section := 0
	for _, packet := range packets {
		if packet.kind == "flush" {
			section++
			continue
		}
		if packet.kind != "data" {
			continue
		}
		line := trimOneNewline(string(packet.payload))
		if section == 0 {
			switch {
			case strings.HasPrefix(line, "want "):
				fields := strings.Split(strings.TrimSpace(line[5:]), " ")
				wants = append(wants, fields[0])
			case strings.HasPrefix(line, "shallow "):
				shallow = append(shallow, strings.TrimSpace(line[8:]))
			case strings.HasPrefix(line, "deepen "):
				n, err := strconv.Atoi(strings.TrimSpace(line[7:]))
				if err != nil {
					n = 0
				}
				depth = &n
			}
		} else if strings.HasPrefix(line, "have ") {
			haves = append(haves, strings.TrimSpace(line[5:]))
		}
	}
	memoKey := g.fetchKey(repo, "v0", wants, haves, shallow, depth)
	if memoized, ok := g.fetchMemo[memoKey]; ok {
		return gitResult("git-upload-pack", "result", memoized), nil
	}
	plan := planFetch(repo, wants, haves, shallow, depth)
	parts := [][]byte{}
	for _, oid := range plan.Shallow {
		parts = append(parts, pktLine("shallow "+oid+"\n"))
	}
	for _, oid := range plan.Unshallow {
		parts = append(parts, pktLine("unshallow "+oid+"\n"))
	}
	parts = append(parts, pktLine("NAK\n"))
	if len(plan.Objects) > 0 {
		pack := writePack(plan.Objects)
		parts = append(parts, sidebandPackets(1, pack)...)
	}
	parts = append(parts, pktFlush)
	return gitResult("git-upload-pack", "result", g.memoizeFetch(memoKey, concatBytes(parts...))), nil
}

var receiveCommandRe = regexp.MustCompile(`^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)$`)

type receiveCommand struct {
	oldOid string
	newOid string
	ref    string
}

func (g *GitHttp) receivePack(owner, name string, body []byte) HttpResult {
	repo := g.store.RepoOf(owner, name)
	if repo == nil {
		return textResult(404, "repository not found\n")
	}
	packets, packBytes, err := parsePkts(body)
	if err != nil {
		return textResult(400, "malformed pkt-line request\n")
	}
	commands := []receiveCommand{}
	capabilities := map[string]bool{}
	for _, packet := range packets {
		if packet.kind != "data" {
			continue
		}
		line := trimOneNewline(string(packet.payload))
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			capabilities = map[string]bool{}
			for _, cap := range strings.Split(line[nul+1:], " ") {
				if cap == "" {
					continue
				}
				name := strings.SplitN(cap, "=", 2)[0]
				capabilities[name] = true
			}
			line = line[:nul]
		}
		if m := receiveCommandRe.FindStringSubmatch(line); m != nil {
			commands = append(commands, receiveCommand{oldOid: m[1], newOid: m[2], ref: m[3]})
		}
	}
	useSideband := capabilities["side-band-64k"]
	useReport := capabilities["report-status"] || capabilities["report-status-v2"]

	var unpackError *string
	if len(packBytes) > 0 {
		objects, err := readPack(packBytes, func(oid string) *GitObject { return repo.Objects[oid] })
		if err != nil {
			msg := err.Error()
			unpackError = &msg
		} else {
			for oid, object := range objects {
				if repo.Objects[oid] == nil {
					repo.Objects[oid] = object
				}
			}
		}
	}

	type refResult struct {
		ref    string
		ok     bool
		reason string
	}
	results := []refResult{}
	for _, command := range commands {
		current, has := repo.Refs[command.ref]
		if !has {
			current = zeroOID
		}
		fail := func(reason string) {
			results = append(results, refResult{ref: command.ref, ok: false, reason: reason})
		}
		if unpackError != nil {
			fail("unpack failed")
			continue
		}
		if command.newOid == zeroOID {
			if current == zeroOID {
				fail("reference does not exist")
			} else if command.oldOid != current {
				fail("failed to lock")
			} else {
				repo.deleteRef(command.ref)
				results = append(results, refResult{ref: command.ref, ok: true})
			}
			continue
		}
		object := repo.Objects[command.newOid]
		if object == nil {
			fail("bad object")
			continue
		}
		if strings.HasPrefix(command.ref, "refs/heads/") && object.Type != "commit" {
			fail("bad object")
			continue
		}
		if current == zeroOID {
			if command.oldOid != zeroOID {
				fail("failed to lock")
			} else {
				repo.setRef(command.ref, command.newOid)
				results = append(results, refResult{ref: command.ref, ok: true})
			}
			continue
		}
		if command.oldOid != current {
			fail("failed to lock")
			continue
		}
		if strings.HasPrefix(command.ref, "refs/heads/") && !isAncestor(repo, current, command.newOid) {
			fail("non-fast-forward")
			continue
		}
		repo.setRef(command.ref, command.newOid)
		results = append(results, refResult{ref: command.ref, ok: true})
	}

	if !useReport {
		return gitResult("git-receive-pack", "result", []byte{})
	}
	reportParts := [][]byte{}
	if unpackError == nil {
		reportParts = append(reportParts, pktLine("unpack ok\n"))
	} else {
		reportParts = append(reportParts, pktLine("unpack "+*unpackError+"\n"))
	}
	for _, result := range results {
		if result.ok {
			reportParts = append(reportParts, pktLine("ok "+result.ref+"\n"))
		} else {
			reportParts = append(reportParts, pktLine("ng "+result.ref+" "+result.reason+"\n"))
		}
	}
	reportParts = append(reportParts, pktFlush)
	report := concatBytes(reportParts...)
	if useSideband {
		wrapped := append(sidebandPackets(1, report), pktFlush)
		return gitResult("git-receive-pack", "result", concatBytes(wrapped...))
	}
	return gitResult("git-receive-pack", "result", report)
}
