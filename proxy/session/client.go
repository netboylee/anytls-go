package session

import (
	"anytls/proxy/padding"
	"anytls/util"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"

	"github.com/chen3feng/stl4go"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/atomic"
)

type Client struct {
	die       context.Context
	dieCancel context.CancelFunc

	dialOut util.DialOutFunc

	sessionCounter  atomic.Uint64
	idleSession     *stl4go.SkipList[uint64, *Session]
	idleSessionLock sync.Mutex

	padding *atomic.TypedValue[*padding.PaddingFactory]

	idleSessionTimeout time.Duration
	minIdleSession     int
}

func NewClient(ctx context.Context, dialOut util.DialOutFunc,
	_padding *atomic.TypedValue[*padding.PaddingFactory], idleSessionCheckInterval, idleSessionTimeout time.Duration, minIdleSession int,
) *Client {
	c := &Client{
		dialOut:            dialOut,
		padding:            _padding,
		idleSessionTimeout: idleSessionTimeout,
		minIdleSession:     minIdleSession,
	}
	if idleSessionCheckInterval <= time.Second*5 {
		idleSessionCheckInterval = time.Second * 30
	}
	if c.idleSessionTimeout <= time.Second*5 {
		c.idleSessionTimeout = time.Second * 30
	}
	c.die, c.dieCancel = context.WithCancel(ctx)
	c.idleSession = stl4go.NewSkipList[uint64, *Session]()
	util.StartRoutine(c.die, idleSessionCheckInterval, c.idleCleanup)
	return c
}

func (c *Client) CreateStream(ctx context.Context) (net.Conn, error) {
	select {
	case <-c.die.Done():
		return nil, io.ErrClosedPipe
	default:
	}

	var session *Session
	var stream *Stream
	var err error

	for i := 0; i < 3; i++ {
		session, err = c.findSession(ctx)
		if session == nil {
			return nil, fmt.Errorf("failed to create session: %w", err)
		}
		stream, err = session.OpenStream()
		if err != nil {
			common.Close(session, stream)
			continue
		}
		break
	}
	if session == nil || stream == nil {
		return nil, fmt.Errorf("too many closed session: %w", err)
	}

	stream.dieHook = func() {
		if session.IsClosed() {
			if session.dieHook != nil {
				session.dieHook()
			}
		} else {
			c.idleSessionLock.Lock()
			session.idleSince = time.Now()
			c.idleSession.Insert(math.MaxUint64-session.seq, session)
			c.idleSessionLock.Unlock()
		}
	}

	return stream, nil
}

func (c *Client) findSession(ctx context.Context) (*Session, error) {
	var idle *Session

	c.idleSessionLock.Lock()
	if !c.idleSession.IsEmpty() {
		it := c.idleSession.Iterate()
		idle = it.Value()
		c.idleSession.Remove(it.Key())
	}
	c.idleSessionLock.Unlock()

	if idle == nil {
		s, err := c.createSession(ctx)
		return s, err
	}
	return idle, nil
}

func (c *Client) createSession(ctx context.Context) (*Session, error) {
	underlying, err := c.dialOut(ctx)
	if err != nil {
		return nil, err
	}

	session := NewClientSession(underlying, &padding.DefaultPaddingFactory)
	session.seq = c.sessionCounter.Add(1)
	session.dieHook = func() {
		//logrus.Debugln("session died", session)
		c.idleSessionLock.Lock()
		c.idleSession.Remove(math.MaxUint64 - session.seq)
		c.idleSessionLock.Unlock()
	}
	session.Run()
	return session, nil
}

func (c *Client) Close() error {
	c.dieCancel()
	go c.idleCleanupExpTime(time.Now())
	return nil
}

func (c *Client) idleCleanup() {
	c.idleCleanupExpTime(time.Now().Add(-c.idleSessionTimeout))
}

func (c *Client) idleCleanupExpTime(expTime time.Time) {
	activeCount := 0
	var sessionToClose []*Session

	c.idleSessionLock.Lock()
	it := c.idleSession.Iterate()
	for it.IsNotEnd() {
		session := it.Value()
		key := it.Key()
		it.MoveToNext()

		if !session.idleSince.Before(expTime) {
			activeCount++
			continue
		}

		if activeCount < c.minIdleSession {
			session.idleSince = time.Now()
			activeCount++
			continue
		}

		sessionToClose = append(sessionToClose, session)
		c.idleSession.Remove(key)
	}
	c.idleSessionLock.Unlock()

	for _, session := range sessionToClose {
		session.Close()
	}
}
