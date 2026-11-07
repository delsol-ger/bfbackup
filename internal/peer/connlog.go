package peer

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"bfbackup/internal/proto"
)

// ConnLogName is the connection log in the peer's data folder.
const ConnLogName = "connection.log"

// ConnLog is a plain text file with every connection attempt, for finding
// out why a peer does not connect. It is capped: when it grows beyond max
// bytes it is renamed to <name>.1 (replacing the previous one) and a new
// file is started.
type ConnLog struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
	now  func() time.Time
}

// OpenConnLog opens (appends to) the log at path.
func OpenConnLog(path string, max int64) (*ConnLog, error) {
	l := &ConnLog{path: path, max: max, now: time.Now}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *ConnLog) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

// Printf writes one timestamped line. Errors writing the log are ignored:
// the log must never stop the peer.
func (l *ConnLog) Printf(format string, args ...any) {
	if l == nil {
		return
	}
	l.printAt(l.now(), fmt.Sprintf(format, args...))
}

func (l *ConnLog) printAt(t time.Time, msg string) {
	if l == nil {
		return
	}
	line := t.Format("2006-01-02 15:04:05.000 -0700") + "  " + msg + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	if l.max > 0 && l.size+int64(len(line)) > l.max {
		l.f.Close()
		os.Rename(l.path, l.path+".1")
		l.f = nil
		if err := l.open(); err != nil {
			return
		}
	}
	n, _ := l.f.WriteString(line)
	l.size += int64(n)
}

// Close closes the file.
func (l *ConnLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Explain turns a connection error into a hint about what to check.
func Explain(err error) string {
	var dns *net.DNSError
	var ne net.Error
	var se *stageErr
	inTLS := errors.As(err, &se) && strings.HasPrefix(se.stage, "setting up tls")
	switch {
	case err == nil:
		return ""
	case inTLS && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET)):
		return "the other side closed the connection during the TLS handshake: the controller probably runs an older BFBackup without encryption. Update the controller, or set BFB_TRANSPORT=plain on this peer until then (or the port belongs to something other than the BFBackup controller)"
	case errors.Is(err, proto.ErrAuth) && strings.Contains(err.Error(), "only encrypted"):
		return "the controller requires encryption: remove BFB_TRANSPORT=plain from this peer"
	case errors.As(err, &dns):
		return "the controller's name does not resolve: check BFB_CONTROLLER, or use the controller's IP address"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "the controller host answered, but nothing listens on that port: is the controller running, and is port 7701 published on its host?"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "no route to the controller: check the address, the network of this container, and that both servers are in the same network"
	case errors.Is(err, proto.ErrControllerAuth):
		return "the server at this address does not know this peer's token: BFB_TOKEN belongs to another controller, or BFB_CONTROLLER points to the wrong server"
	case errors.Is(err, proto.ErrAuth) && strings.Contains(err.Error(), "unknown node"):
		return "the controller does not know this peer: it was removed, or the configuration was imported without it. Add the peer again in the web UI and use the new token"
	case errors.Is(err, proto.ErrAuth) && strings.Contains(err.Error(), "protocol version"):
		return "the controller and this peer run incompatible versions: update both containers"
	case errors.Is(err, proto.ErrAuth) && strings.Contains(err.Error(), "invalid token"):
		return "the token is outdated: a new token was created in the web UI (Peers → New token). Put the new token into BFB_TOKEN"
	case errors.Is(err, proto.ErrAuth):
		return "the controller refused this peer: check BFB_TOKEN"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET):
		return "the connection was closed by the other side: during connecting this usually means the port belongs to something other than the BFBackup controller (port 7701 is for peers, 7700 is the web UI)"
	case errors.As(err, &ne) && ne.Timeout():
		return "no answer in time: wrong address, a firewall drops the packets, or the controller host is switched off"
	}
	return ""
}
