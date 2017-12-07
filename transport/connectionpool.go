package transport

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/ghettovoice/gosip/core"
	"github.com/ghettovoice/gosip/log"
	"github.com/ghettovoice/gosip/syntax"
	"github.com/ghettovoice/gosip/timing"
)

type ConnKey net.Addr

// ConnectionPool used for active connection management.
type ConnectionPool interface {
	log.WithLogger
	String() string
	Add(key ConnKey, connection Connection, ttl time.Duration) error
	Get(key ConnKey) (Connection, bool)
	Drop(key ConnKey) bool
	Serve()
}

// ConnectionHandler serves associated connection, i.e. parses
// incoming data, manages expiry time & etc.
type ConnectionHandler interface {
	log.WithLogger
	String() string
	Key() ConnKey
	Connection() Connection
	// Expiries returns connection expiry time.
	Expiries() time.Time
	// Update updates connection expiry time.
	Update(ttl time.Duration)
	// Serve runs connection serving.
	Serve()
}

type CancellableConnectionHandler interface {
	ConnectionHandler
	Cancel()
}

// Thread-safe connection pool implementation with expiry management.
type connectionPool struct {
	ctx             context.Context
	log             log.Logger
	lock            *sync.RWMutex
	wg              *sync.WaitGroup
	store           map[ConnKey]ConnectionHandler
	expiredHandlers chan ConnectionHandler
	output          chan<- *IncomingMessage
	errs            chan<- error
	handlerErrors   chan error
}

func NewConnectionPool(ctx context.Context, output chan<- *IncomingMessage, errs chan<- error) *connectionPool {
	pool := &connectionPool{
		ctx:             ctx,
		lock:            new(sync.RWMutex),
		wg:              new(sync.WaitGroup),
		store:           make(map[ConnKey]ConnectionHandler),
		expiredHandlers: make(chan ConnectionHandler),
		handlerErrors:   make(chan error),
		output:          output,
		errs:            errs,
	}
	pool.SetLog(log.StandardLogger())
	return pool
}

func (pool *connectionPool) String() string {
	var name string
	if pool == nil {
		name = "<nil>"
	} else {
		name = fmt.Sprintf("%p", pool)
	}

	return fmt.Sprintf("connection pool %s", name)
}

func (pool *connectionPool) Log() log.Logger {
	return pool.log
}

func (pool *connectionPool) SetLog(logger log.Logger) {
	pool.log = logger.WithField("conn-pool", pool.String())
}

func (pool *connectionPool) Add(key ConnKey, connection Connection, ttl time.Duration) error {
	if pool.ctx.Err() != nil {
		return pool.ctx.Err()
	}

	handler, ok := pool.getHandler(key)
	if !ok {
		ctx, cancel := context.WithCancel(pool.ctx)
		handler := NewConnectionHandler(ctx, key, connection, ttl, pool.expiredHandlers, pool.output, pool.handlerErrors)
		pool.addHandler(key, NewCancellableConnectionHandler(handler, cancel))
		pool.wg.Add(1)
		go func() {
			defer pool.wg.Done()
			handler.Serve()
		}()
	} else {
		handler.Update(ttl)
	}

	return nil
}

func (pool *connectionPool) Get(key ConnKey) (Connection, bool) {
	if handler, ok := pool.getHandler(key); ok {
		return handler.Connection(), true
	} else {
		return nil, false
	}
}

func (pool *connectionPool) Drop(key ConnKey) bool {
	if handler, ok := pool.getHandler(key); ok {
		if handler, ok := handler.(CancellableConnectionHandler); ok {
			handler.Cancel()
		}
		pool.dropHandler(key)
		return true
	}

	return false
}

// Serve serves registered connections: expires, termination nd etc.
func (pool *connectionPool) Serve() {
	defer func() {
		pool.Log().Infof("stop %s serving", pool)
		pool.dispose()
	}()
	pool.Log().Infof("start %s serving", pool)

	for {
		select {
		case <-pool.ctx.Done():
			return
		case handler := <-pool.expiredHandlers:
			_, ok := pool.getHandler(handler.Key())
			if !ok {
				pool.Log().Warnf("ignore already dropped out %s in %s", handler, pool)
				continue
			}

			if handler.Expiries().Before(time.Now()) {
				// connection expired
				pool.Log().Debugf("%s notified that %s has expired, drop it", pool, handler)
				// close and drop from pool
				pool.Drop(handler.Key())
			} else {
				// Due to a race condition, the socket has been updated since this expiry happened.
				// Ignore the expiry since we already have a new socket for this address.
				pool.Log().Warnf("ignored spurious expiry of %s in %s", handler, pool)
			}
		}
	}
}

func (pool *connectionPool) addHandler(key ConnKey, connHandler ConnectionHandler) {
	pool.lock.Lock()
	pool.store[key] = connHandler
	pool.lock.Unlock()
}

func (pool *connectionPool) getHandler(key ConnKey) (ConnectionHandler, bool) {
	pool.lock.RLock()
	defer pool.lock.RUnlock()
	handler, ok := pool.store[key]
	return handler, ok
}

func (pool *connectionPool) dropHandler(key ConnKey) {
	pool.lock.Lock()
	delete(pool.store, key)
	pool.lock.Unlock()
}

func (pool *connectionPool) allHandlers() []ConnectionHandler {
	all := make([]ConnectionHandler, 0)
	for key := range pool.store {
		if handler, ok := pool.getHandler(key); ok {
			all = append(all, handler)
		}
	}

	return all
}

func (pool *connectionPool) dispose() {
	pool.Log().Debugf("dispose %s", pool)
	for _, handler := range pool.allHandlers() {
		pool.Drop(handler.Key())
	}
	pool.wg.Wait()
	close(pool.expiredHandlers)
	close(pool.handlerErrors)
}

// connectionHandler actually serves associated connection
type connectionHandler struct {
	log        log.Logger
	ctx        context.Context
	key        ConnKey
	connection Connection
	timer      timing.Timer
	expiryTime time.Time
	expired    chan<- ConnectionHandler
	output     chan<- *IncomingMessage
	errs       chan<- error
}

func NewConnectionHandler(
	ctx context.Context,
	key ConnKey,
	conn Connection,
	ttl time.Duration,
	expired chan<- ConnectionHandler,
	output chan<- *IncomingMessage,
	errs chan<- error,
) ConnectionHandler {
	handler := &connectionHandler{
		ctx:        ctx,
		key:        key,
		connection: conn,
		expired:    expired,
		timer:      timing.NewTimer(ttl),
		expiryTime: timing.Now().Add(ttl),
		output:     output,
		errs:       errs,
	}
	handler.SetLog(conn.Log())
	return handler
}

func (handler *connectionHandler) String() string {
	var name, key, conn, addition string
	if handler == nil {
		name = "<nil>"
		key = ""
		conn = ""
	} else {
		name = fmt.Sprintf("%p", handler)
		if handler.Key() != nil {
			key = fmt.Sprintf("%s", handler.Key())
		}
		if handler.Connection() != nil {
			conn = fmt.Sprintf("%s", handler.Connection())
		}
		if key != "" || conn != "" {
			addition = "("
			if key != "" {
				addition += key
			}
			if conn != "" {
				addition += conn
			}
			addition += ")"
		}
	}

	return fmt.Sprintf("connection handler %s%s", name, addition)
}

func (handler *connectionHandler) Log() log.Logger {
	return handler.log
}

func (handler *connectionHandler) SetLog(logger log.Logger) {
	handler.log = logger.WithFields(map[string]interface{}{
		"conn-handler": handler.String(),
		"conn-key":     fmt.Sprintf("%s", handler.Key()),
		"conn":         fmt.Sprintf("%s", handler.Connection()),
	})
}

func (handler *connectionHandler) Key() ConnKey {
	return handler.key
}

func (handler *connectionHandler) Connection() Connection {
	return handler.connection
}

func (handler *connectionHandler) Expiries() time.Time {
	return handler.expiryTime
}

// resets the timeout timer.
func (handler *connectionHandler) Update(ttl time.Duration) {
	handler.expiryTime = timing.Now().Add(ttl)
	handler.Log().Debugf("update expiry time of %s for key %s and %s to %s", handler, handler.Key(),
		handler.Connection(), handler.Expiries())
	handler.timer.Reset(ttl)
}

// connection serving loop.
// Waits for the connection to expire, and notifies the pool when it does.
func (handler *connectionHandler) Serve() {
	parsedMessages := make(chan core.Message)
	parserErrors := make(chan error)
	parser := syntax.NewParser(parsedMessages, parserErrors, handler.Connection().IsStream())
	parser.SetLog(handler.Log())

	defer func() {
		handler.Log().Infof("stop serving %s for key %s and %s", handler, handler.Key(), handler.Connection())
		parser.Stop()
		handler.dispose()
		close(parsedMessages)
		close(parserErrors)
	}()

	handler.Log().Infof("begin serving %s for key %s and %s", handler, handler.Key(), handler.Connection())

	buf := make([]byte, bufferSize)
	for {
		select {
		// pool canceled current handler
		case <-handler.ctx.Done():
			return
			// connection expired
		case <-handler.timer.C():
			handler.Log().Debugf("%s with key %s inactive for too long, so close it", handler.Connection(),
				handler.Key())
			// pass up to the pool expired connection key
			// pool will make decision to drop out connection or update ttl.
			select {
			case <-handler.ctx.Done():
				return
			case handler.expired <- handler.Key():
			}
		case msg, ok := <-parsedMessages:
			if !ok {
				// connection was closed, exit
				return
			}
			if msg != nil {
				handler.Log().Infof("%s received message '%s' from %s and %s, passing it up", handler,
					msg.Short(), handler.Connection(), parser)

				incomingMsg := &IncomingMessage{
					msg,
					handler.Connection().LocalAddr(),
					handler.Connection().RemoteAddr(),
				}
				select {
				case <-handler.ctx.Done(): // protocol stop called
					return
				case handler.output <- incomingMsg:
					handler.Log().Debugf("%s passed up message '%s' %p from %s and %s", handler, msg.Short(),
						msg, handler.Connection(), parser)
				}
			}
		case err, ok := <-parserErrors:
			if !ok {
				// connection was closed, exit
				return
			}
			if err != nil {
				// on parser errors (should be syntax.Error) just reset parser and pass the error up
				handler.Log().Warnf("%s reset %s for %s due to parser error: %s", handler, parser,
					handler.Connection(), err)
				parser.Reset()

				select {
				case <-handler.ctx.Done():
					return
				case handler.errs <- &ConnectionHandlerError{err, handler}:
					handler.Log().Debugf("%s passed up unhandled error %s from %s and %s", handler, err,
						handler.Connection(), parser)
				}
			}
		default:
			num, err := handler.Connection().Read(buf)
			if err != nil {
				// if we get timeout error just go further and try read on the next iteration
				if err, ok := err.(net.Error); ok {
					if err.Timeout() || err.Temporary() {
						handler.Log().Debugf("%s timeout or temporary unavailable, sleep by %d seconds",
							handler.Connection(), netErrRetryTime)
						time.Sleep(netErrRetryTime)
						continue
					}
				}
				// broken or closed connection, stop reading and piping
				// and pass up error (net.Error)
				select {
				case <-handler.ctx.Done():
				case handler.errs <- &ConnectionHandlerError{err, handler}:
					handler.Log().Debugf("%s passed up unhandled error %s from %s and %s", handler, err,
						handler.Connection(), parser)
				}
				return
			}

			pkt := append([]byte{}, buf[:num]...)
			if _, err := parser.Write(pkt); err != nil {
				select {
				case <-handler.ctx.Done():
					return
				case parserErrors <- err:
				}
			}
		}
	}
}

func (handler *connectionHandler) dispose() {
	handler.Log().Debugf("dispose %s for key %s and close %s", handler, handler.Key(), handler.Connection())
	handler.timer.Stop()
	handler.Connection().Close()
}

// ConnectionHandler decorator that implements CancellableConnectionHandler interface.
type cancellableConnectionHandler struct {
	ConnectionHandler
	cancel func()
}

func NewCancellableConnectionHandler(handler ConnectionHandler, cancel func()) CancellableConnectionHandler {
	return &cancellableConnectionHandler{handler, cancel}
}

// Cancel simply calls runtime provided cancel function.
func (handler *cancellableConnectionHandler) Cancel() {
	handler.cancel()
}
