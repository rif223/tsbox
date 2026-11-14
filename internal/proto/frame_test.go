package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := []*Frame{
		{T: TCall, ID: 7, SDK: "kv", M: "get", P: json.RawMessage(`{"key":"a"}`)},
		{T: TEvent, SIDs: []string{"s1", "s2"}, SDK: "chat", Name: "message", P: json.RawMessage(`{"text":"hi"}`)},
		{T: TLog, SID: "s2", Level: "info", Msg: "hello 👋"},
		{T: TRet, SID: "s1", ID: 7, Err: &Error{Code: "not_found"}},
	}
	for _, f := range in {
		if err := Write(&buf, f); err != nil {
			t.Fatal(err)
		}
	}
	for i := range in {
		f, err := Read(&buf)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(in[i])
		b, _ := json.Marshal(f)
		if !bytes.Equal(a, b) {
			t.Errorf("Frame %d: %s != %s", i, b, a)
		}
	}
	if _, err := Read(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("end: %v", err)
	}
}

func frame(body string) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	return append(b, body...)
}

func TestReadRejects(t *testing.T) {
	cases := map[string][]byte{
		"empty":         {0, 0, 0, 0},
		"too large":     {0x00, 0x10, 0x00, 0x01},
		"not JSON":      frame("nope"),
		"unknown":       frame(`{"t":"load"}`),
		"unknown field": frame(`{"t":"log","x":1}`),
		"trailing data": frame(`{"t":"log"}{"t":"log"}`),
		"ID too large":  frame(`{"t":"call","id":9007199254740992}`),
		"truncated":     frame(`{"t":"log"}`)[:8],
	}
	for name, b := range cases {
		if _, err := Read(bytes.NewReader(b)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestPayloadStrict(t *testing.T) {
	var s Start
	if err := Payload(&Frame{T: TStart, P: json.RawMessage(`{"artifact":"a","sha256":"b","heapMiB":1,"evil":1}`)}, &s); err == nil {
		t.Fatal("unknown field accepted")
	}
	if err := Payload(&Frame{T: TStart}, &s); err == nil {
		t.Fatal("missing payload accepted")
	}
}

func TestFromScriptSubsetOfKnown(t *testing.T) {
	for typ := range FromScript {
		if !knownTypes[typ] {
			t.Errorf("%s unknown", typ)
		}
	}
	for _, forbidden := range []string{TStart, TStop, TExit, THello, TPStats, TRet, TEvent} {
		if FromScript[forbidden] {
			t.Errorf("a script must not send %s", forbidden)
		}
	}
	if FromInit[TAlive] {
		t.Error("alive must not reach the host")
	}
}

// FuzzRead: invariant I5 – no input crashes the decoder, and every accepted frame can be encoded
// again.
func FuzzRead(f *testing.F) {
	f.Add(frame(`{"t":"call","id":1,"sdk":"kv","m":"get","p":{}}`))
	f.Add(frame(`{"t":"ret","id":1,"err":{"code":"x"}}`))
	f.Add(frame(`{"t":"log","level":"info","msg":"x"}`))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add(frame(strings.Repeat("[", 1000)))
	f.Fuzz(func(t *testing.T, b []byte) {
		r := bytes.NewReader(b)
		for {
			fr, err := Read(r)
			if err != nil {
				return
			}
			if _, err := Encode(fr); err != nil && !errors.Is(err, ErrTooLarge) {
				t.Fatalf("Encode: %v", err)
			}
		}
	})
}

// FuzzPayload: the payloads of all frame types coming from init or from the box are decoded strictly and
// without crashing.
func FuzzPayload(f *testing.F) {
	f.Add(`{"proto":2,"init":"x","uidMode":true,"baselineRss":1}`)
	f.Add(`{"artifact":"/a","sha256":"ab","uid":10001,"heapMiB":64,"params":{"x":[1]}}`)
	f.Add(`{"code":1,"signal":"SIGKILL","reason":"rss","detail":"d"}`)
	f.Add(`[{"sid":"main","rss":1,"cpuNs":2,"threads":3}]`)
	f.Add(`{"heapUsed":1,"heapTotal":2,"external":3}`)
	f.Fuzz(func(t *testing.T, p string) {
		fr := &Frame{T: "x", P: json.RawMessage(p)}
		var (
			h  Hello
			st Start
			ex Exit
			ps []ProcStats
			ss ScriptStats
			e  Error
		)
		for _, v := range []any{&h, &st, &ex, &ps, &ss, &e} {
			if Payload(fr, v) == nil {
				if _, err := json.Marshal(v); err != nil {
					t.Fatalf("%T: %v", v, err)
				}
			}
		}
	})
}
