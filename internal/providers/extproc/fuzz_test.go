package extproc

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"testing"

	"github.com/rif223/tsbox/internal/proto"
	"github.com/rif223/tsbox/internal/sdktest"
	"github.com/rif223/tsbox/sdk"
)

func frames(fs ...string) []byte {
	var b []byte
	for _, f := range fs {
		b = binary.BigEndian.AppendUint32(b, uint32(len(f)))
		b = append(b, f...)
	}
	return b
}

// FuzzReadLoop: frames of a (faulty) provider process do not crash the engine; open calls and opens end
// at the latest with the stream.
func FuzzReadLoop(f *testing.F) {
	f.Add(frames(`{"t":"init_ok","p":{}}`, `{"t":"opened","sid":"1"}`, `{"t":"ret","sid":"1","id":1,"p":{"x":1}}`,
		`{"t":"emit","sid":"1","name":"tick","p":1}`, `{"t":"log","level":"info","msg":"m"}`, `{"t":"pong","id":1}`))
	f.Add(frames(`{"t":"ret","id":1,"err":{"code":"x","msg":"y"}}`, `{"t":"start","sid":"a"}`))
	f.Add([]byte{0, 0, 0, 1, '{'})
	f.Fuzz(func(t *testing.T, b []byte) {
		p := New(Config{Name: "fuzz", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		ret, open := make(chan *proto.Frame, 1), make(chan *proto.Frame, 1)
		pr := &proc{pending: map[uint64]chan *proto.Frame{1: ret}, opens: map[string]chan *proto.Frame{"1": open},
			sess: map[string]sdk.Session{"1": &sdktest.Session{}}, done: make(chan struct{}), ready: make(chan error, 1)}
		p.readLoop(pr, bytes.NewReader(b))
		<-pr.done
		<-ret // delivered or closed
		<-open
	})
}
