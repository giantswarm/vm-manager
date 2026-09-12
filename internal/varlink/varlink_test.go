package varlink_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/varlink"
	"github.com/giantswarm/vm-manager/internal/varlink/varlinktest"
)

const testIface = "io.test.Echo"

func dial(t *testing.T, srv *varlinktest.Server) *varlink.Conn {
	t.Helper()
	conn, err := varlink.Dial(context.Background(), srv.Path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestCallDecodesReplyAndRecordsRequest(t *testing.T) {
	srv := varlinktest.New(t, func(c varlinktest.Call) []varlinktest.Reply {
		var p struct {
			Text string `json:"text"`
		}
		require.NoError(t, json.Unmarshal(c.Parameters, &p))
		return []varlinktest.Reply{{Parameters: map[string]any{"echo": p.Text, "n": 3}}}
	})
	conn := dial(t, srv)

	var out struct {
		Echo string `json:"echo"`
		N    int    `json:"n"`
	}
	err := conn.Call(context.Background(), testIface+".Say", map[string]string{"text": "hi"}, &out)
	require.NoError(t, err)
	assert.Equal(t, "hi", out.Echo)
	assert.Equal(t, 3, out.N)

	calls := srv.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, testIface+".Say", calls[0].Method)
	assert.False(t, calls[0].More)
	assert.False(t, calls[0].Oneway)

	// Nil params are sent as an empty object, nil out discards the reply,
	// and the connection is still usable after a normal call.
	require.NoError(t, conn.Call(context.Background(), testIface+".Say", nil, nil))
	assert.JSONEq(t, `{}`, string(srv.Calls()[1].Parameters))
}

func TestCallErrorReplyKeepsConnectionUsable(t *testing.T) {
	srv := varlinktest.New(t, func(c varlinktest.Call) []varlinktest.Reply {
		if c.Method == testIface+".Fail" {
			return []varlinktest.Reply{{Error: testIface + ".Boom", Parameters: map[string]string{"why": "because"}}}
		}
		return nil
	})
	conn := dial(t, srv)

	err := conn.Call(context.Background(), testIface+".Fail", nil, nil)
	var verr *varlink.Error
	require.ErrorAs(t, err, &verr)
	assert.Equal(t, testIface+".Boom", verr.Name)
	assert.JSONEq(t, `{"why":"because"}`, string(verr.Parameters))
	assert.Contains(t, verr.Error(), "Boom")

	require.NoError(t, conn.Call(context.Background(), testIface+".Ok", nil, nil))
}

func TestCallMoreStreamsUntilLastReply(t *testing.T) {
	srv := varlinktest.New(t, func(varlinktest.Call) []varlinktest.Reply {
		return []varlinktest.Reply{
			{Parameters: map[string]int{"i": 1}},
			{Parameters: map[string]int{"i": 2}},
			{Parameters: map[string]int{"i": 3}},
		}
	})
	conn := dial(t, srv)

	var got []int
	err := conn.CallMore(context.Background(), testIface+".List", nil, func(raw json.RawMessage) error {
		var p struct {
			I int `json:"i"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		got = append(got, p.I)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, got)
	assert.True(t, srv.Calls()[0].More)

	// A callback error before the last reply abandons the stream and the
	// connection with it; one on the last reply does not.
	stop := errors.New("stop")
	err = conn.CallMore(context.Background(), testIface+".List", nil, func(json.RawMessage) error { return stop })
	require.ErrorIs(t, err, stop)
	require.ErrorIs(t, conn.Call(context.Background(), testIface+".List", nil, nil), varlink.ErrClosed)

	conn = dial(t, srv)
	n := 0
	err = conn.CallMore(context.Background(), testIface+".List", nil, func(json.RawMessage) error {
		n++
		if n == 3 {
			return stop
		}
		return nil
	})
	require.ErrorIs(t, err, stop)
	require.NoError(t, conn.Call(context.Background(), testIface+".List", nil, nil))
}

func TestCallMoreErrorReplyEndsStream(t *testing.T) {
	srv := varlinktest.New(t, func(varlinktest.Call) []varlinktest.Reply {
		return []varlinktest.Reply{{Parameters: map[string]int{"i": 1}}, {Error: testIface + ".Boom"}}
	})
	conn := dial(t, srv)

	n := 0
	err := conn.CallMore(context.Background(), testIface+".List", nil, func(json.RawMessage) error { n++; return nil })
	var verr *varlink.Error
	require.ErrorAs(t, err, &verr)
	assert.Equal(t, 1, n)
}

func TestCallWithFilesReceivesDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.WriteFile(path, []byte("payload"), 0o600))

	srv := varlinktest.New(t, func(varlinktest.Call) []varlinktest.Reply {
		f, err := os.Open(path) // #nosec G304 -- test fixture path
		require.NoError(t, err)
		return []varlinktest.Reply{{Parameters: map[string]int{"fileDescriptorIndex": 0}, Files: []*os.File{f}}}
	})
	conn := dial(t, srv)

	var out struct {
		Index int `json:"fileDescriptorIndex"`
	}
	files, err := conn.CallWithFiles(context.Background(), testIface+".Open", nil, &out)
	require.NoError(t, err)
	require.Len(t, files, 1)
	volume := files[out.Index]
	defer func() { _ = volume.Close() }()

	data, err := io.ReadAll(volume)
	require.NoError(t, err)
	assert.Equal(t, "payload", string(data))

	// Call closes stray descriptors; a following call must not inherit them.
	require.NoError(t, conn.Call(context.Background(), testIface+".Open", nil, nil))
	files, err = conn.CallWithFiles(context.Background(), "org.varlink.service.GetInfo", nil, nil)
	require.NoError(t, err)
	assert.Empty(t, files)
}

func TestOnewaySendsWithoutReply(t *testing.T) {
	srv := varlinktest.New(t, nil)
	conn := dial(t, srv)

	require.NoError(t, conn.Oneway(context.Background(), testIface+".Ping", map[string]int{"seq": 1}))
	// A regular call afterwards proves the oneway left no stray reply on the wire.
	info, err := conn.Info(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "varlinktest", info.Product)

	calls := srv.Calls()
	require.Len(t, calls, 2)
	assert.True(t, calls[0].Oneway)
}

func TestInfoAndInterfaceDescription(t *testing.T) {
	srv := varlinktest.New(t, nil)
	srv.Interfaces[testIface] = "interface io.test.Echo\nmethod Say(text: string) -> (echo: string)\n"
	conn := dial(t, srv)

	info, err := conn.Info(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"org.varlink.service", testIface}, info.Interfaces)

	desc, err := conn.InterfaceDescription(context.Background(), testIface)
	require.NoError(t, err)
	assert.Equal(t, srv.Interfaces[testIface], desc)

	_, err = conn.InterfaceDescription(context.Background(), "io.test.Missing")
	var verr *varlink.Error
	require.ErrorAs(t, err, &verr)
	assert.Equal(t, "org.varlink.service.InterfaceNotFound", verr.Name)

	err = conn.Call(context.Background(), testIface+".Say", nil, nil)
	require.ErrorAs(t, err, &verr)
	assert.Equal(t, "org.varlink.service.MethodNotFound", verr.Name)
}

func TestContextCancelBreaksConnection(t *testing.T) {
	block := make(chan struct{})
	srv := varlinktest.New(t, func(varlinktest.Call) []varlinktest.Reply {
		<-block
		return nil
	})
	t.Cleanup(func() { close(block) })
	conn := dial(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := conn.Call(ctx, testIface+".Hang", nil, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, conn.Call(context.Background(), testIface+".Hang", nil, nil), varlink.ErrClosed)
}

func TestDialMissingSocket(t *testing.T) {
	_, err := varlink.Dial(context.Background(), filepath.Join(t.TempDir(), "missing.sock"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// The descriptor sent with the second of three streamed replies must be
// handed to that reply only, not to whichever message is decoded first.
func TestCallMoreAttributesFilesToTheirReply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.WriteFile(path, []byte("second"), 0o600))

	srv := varlinktest.New(t, func(varlinktest.Call) []varlinktest.Reply {
		f, err := os.Open(path) // #nosec G304 -- test fixture path
		require.NoError(t, err)
		return []varlinktest.Reply{
			{Parameters: map[string]int{"n": 1}},
			{Parameters: map[string]int{"n": 2}, Files: []*os.File{f}},
			{Parameters: map[string]int{"n": 3}},
		}
	})
	conn := dial(t, srv)

	var seen []int
	err := conn.CallMoreWithFiles(context.Background(), testIface+".Stream", nil, func(raw json.RawMessage, files []*os.File) error {
		var p struct {
			N int `json:"n"`
		}
		require.NoError(t, json.Unmarshal(raw, &p))
		seen = append(seen, p.N)
		if p.N == 2 {
			require.Len(t, files, 1, "reply 2 carries the descriptor")
			data, err := io.ReadAll(files[0])
			require.NoError(t, err)
			assert.Equal(t, "second", string(data))
		} else {
			assert.Empty(t, files, "reply %d carries no descriptor", p.N)
		}
		closeAll(files)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, seen)
}

func closeAll(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}
