package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const zeroOID = "0000000000000000000000000000000000000000"

func sha1Hex(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}

// gitOid is the git object id: sha1 of "<type> <size>\0" + content.
func gitOid(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func b64encode(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

func b64decode(text string) []byte {
	clean := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' || r == ' ' {
			return -1
		}
		return r
	}, text)
	b, _ := base64.StdEncoding.DecodeString(clean)
	return b
}

// nodeId is a GitHub-style global node id: base64 of "010:<Type><databaseId>".
func nodeId(typ string, databaseId int) string {
	return base64.StdEncoding.EncodeToString([]byte("010:" + typ + strconv.Itoa(databaseId)))
}

var nodeIDRe = regexp.MustCompile(`^010:([A-Za-z]+)(\d+)$`)

type nodeIDDecoded struct {
	Type       string
	DatabaseID int
}

func decodeNodeId(id string) *nodeIDDecoded {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	m := nodeIDRe.FindStringSubmatch(string(raw))
	if m == nil {
		return nil
	}
	n, _ := strconv.Atoi(m[2])
	return &nodeIDDecoded{Type: m[1], DatabaseID: n}
}

var sha40Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

func isSha40(s string) bool { return sha40Re.MatchString(strings.ToLower(s)) }

var reqCounter uint64

func nextRequestId() string {
	n := atomic.AddUint64(&reqCounter, 1)
	s := strconv.FormatUint(n, 16)
	for len(s) < 8 {
		s = "0" + s
	}
	return s
}

// The fixed trial clock: every timestamp derives from MAKEFUN_TRIAL_NOW, never wall time.
var trialNowCached = func() string {
	if v := os.Getenv("MAKEFUN_TRIAL_NOW"); v != "" {
		return v
	}
	return "2026-11-14T06:30:00.000Z"
}()

func trialNow() string { return trialNowCached }

var trialEpochCached = func() int64 {
	t, err := time.Parse(time.RFC3339Nano, trialNowCached)
	if err != nil {
		return 1763101800
	}
	return t.Unix()
}()

func trialEpoch() int64 { return trialEpochCached }
