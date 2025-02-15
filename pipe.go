package rueidis

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rueian/rueidis/internal/cmds"
)

var noHello = regexp.MustCompile("unknown command .HELLO.")

type wire interface {
	Do(ctx context.Context, cmd cmds.Completed) RedisResult
	DoCache(ctx context.Context, cmd cmds.Cacheable, ttl time.Duration) RedisResult
	DoMulti(ctx context.Context, multi ...cmds.Completed) []RedisResult
	DoMultiCache(ctx context.Context, multi ...CacheableTTL) []RedisResult
	Receive(ctx context.Context, subscribe cmds.Completed, fn func(message PubSubMessage)) error
	Info() map[string]RedisMessage
	Error() error
	Close()

	CleanSubscriptions()
	SetPubSubHooks(hooks PubSubHooks) <-chan error
}

var _ wire = (*pipe)(nil)

type pipe struct {
	waits   int32
	state   int32
	version int32
	blcksig int32
	timeout time.Duration
	pinggap time.Duration

	r *bufio.Reader
	w *bufio.Writer

	conn  net.Conn
	cache cache
	queue queue
	once  sync.Once

	info  map[string]RedisMessage
	nsubs *subs
	psubs *subs
	ssubs *subs
	pshks atomic.Value
	error atomic.Value

	onInvalidations func([]RedisMessage)
}

func newPipe(conn net.Conn, option *ClientOption) (p *pipe, err error) {
	p = &pipe{
		conn:  conn,
		queue: newRing(option.RingScaleEachConn),
		r:     bufio.NewReaderSize(conn, option.ReadBufferEachConn),
		w:     bufio.NewWriterSize(conn, option.WriteBufferEachConn),

		nsubs: newSubs(),
		psubs: newSubs(),
		ssubs: newSubs(),

		timeout: option.ConnWriteTimeout,
		pinggap: option.Dialer.KeepAlive,
	}
	if !option.DisableCache {
		p.cache = newLRU(option.CacheSizeEachConn)
	}
	p.pshks.Store(emptypshks)

	helloCmd := []string{"HELLO", "3"}
	if option.Password != "" && option.Username == "" {
		helloCmd = append(helloCmd, "AUTH", "default", option.Password)
	} else if option.Username != "" {
		helloCmd = append(helloCmd, "AUTH", option.Username, option.Password)
	}
	if option.ClientName != "" {
		helloCmd = append(helloCmd, "SETNAME", option.ClientName)
	}

	init := make([][]string, 0, 3)
	if option.ClientTrackingOptions == nil {
		init = append(init, helloCmd, []string{"CLIENT", "TRACKING", "ON", "OPTIN"})
	} else {
		init = append(init, helloCmd, append([]string{"CLIENT", "TRACKING", "ON"}, option.ClientTrackingOptions...))
	}
	if option.DisableCache {
		init = init[:1]
	}
	if option.SelectDB != 0 {
		init = append(init, []string{"SELECT", strconv.Itoa(option.SelectDB)})
	}

	timeout := option.Dialer.Timeout
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var resp2 bool
	for i, r := range p.DoMulti(ctx, cmds.NewMultiCompleted(init)...) {
		if i == 0 {
			p.info, err = r.ToMap()
		} else {
			err = r.Error()
		}
		if err != nil {
			if re, ok := err.(*RedisError); ok {
				if !resp2 && noHello.MatchString(re.string) {
					resp2 = true
					continue
				} else if strings.Contains(re.string, "wrong number of arguments for 'TRACKING'") {
					err = fmt.Errorf("%s: %w", re.string, ErrNoCache)
				} else if resp2 {
					continue
				}
			}
			p.Close()
			return nil, err
		}
	}
	if !resp2 {
		if ver, ok := p.info["version"]; ok {
			if v := strings.Split(ver.string, "."); len(v) != 0 {
				vv, _ := strconv.ParseInt(v[0], 10, 32)
				p.version = int32(vv)
			}
		}
		if p.onInvalidations = option.OnInvalidations; p.onInvalidations != nil {
			p.background()
		}
	} else {
		if !option.DisableCache {
			p.Close()
			return nil, ErrNoCache
		}
		init = init[:0]
		if option.Password != "" && option.Username == "" {
			init = append(init, []string{"AUTH", option.Password})
		} else if option.Username != "" {
			init = append(init, []string{"AUTH", option.Username, option.Password})
		}
		if option.ClientName != "" {
			init = append(init, []string{"CLIENT", "SETNAME", option.ClientName})
		}
		if option.SelectDB != 0 {
			init = append(init, []string{"SELECT", strconv.Itoa(option.SelectDB)})
		}
		if len(init) != 0 {
			for _, r := range p.DoMulti(ctx, cmds.NewMultiCompleted(init)...) {
				if err = r.Error(); err != nil {
					p.Close()
					return nil, err
				}
			}
		}
		p.version = 5
	}
	return p, nil
}

func (p *pipe) background() {
	atomic.CompareAndSwapInt32(&p.state, 0, 1)
	p.once.Do(func() { go p._background() })
}

func (p *pipe) _background() {
	exit := func(err error) {
		p.error.CompareAndSwap(nil, &errs{error: err})
		atomic.CompareAndSwapInt32(&p.state, 1, 2) // stop accepting new requests
		_ = p.conn.Close()                         // force both read & write goroutine to exit
	}
	wait := make(chan struct{})
	if p.timeout > 0 && p.pinggap > 0 {
		go func() {
			if err := p._backgroundPing(wait); err != ErrClosing {
				exit(err)
			}
		}()
	}
	go func() {
		exit(p._backgroundWrite())
		close(wait)
	}()
	{
		exit(p._backgroundRead())
		atomic.CompareAndSwapInt32(&p.state, 2, 3) // make write goroutine to exit
		atomic.AddInt32(&p.waits, 1)
		go func() {
			<-p.queue.PutOne(cmds.QuitCmd)
			atomic.AddInt32(&p.waits, -1)
		}()
	}
	<-wait

	p.nsubs.Close()
	p.psubs.Close()
	p.ssubs.Close()
	if old := p.pshks.Swap(emptypshks).(*pshks); old.close != nil {
		old.close <- p.Error()
		close(old.close)
	}

	var (
		ones  = make([]cmds.Completed, 1)
		multi []cmds.Completed
		ch    chan RedisResult
		cond  *sync.Cond
	)

	// clean up cache and free pending calls
	if p.cache != nil {
		p.cache.FreeAndClose(RedisMessage{typ: '-', string: p.Error().Error()})
	}
	if p.onInvalidations != nil {
		p.onInvalidations(nil)
	}
	for atomic.LoadInt32(&p.waits) != 0 {
		p.queue.NextWriteCmd()
		if ones[0], multi, ch, cond = p.queue.NextResultCh(); ch != nil {
			if multi == nil {
				multi = ones
			}
			for range multi {
				ch <- newErrResult(p.Error())
			}
			cond.L.Unlock()
			cond.Signal()
		} else {
			cond.L.Unlock()
			cond.Signal()
			runtime.Gosched()
		}
	}
	atomic.StoreInt32(&p.state, 4)
}

func (p *pipe) _backgroundWrite() (err error) {
	var (
		ones  = make([]cmds.Completed, 1)
		multi []cmds.Completed
		ch    chan RedisResult
	)

	for atomic.LoadInt32(&p.state) < 3 {
		if ones[0], multi, ch = p.queue.NextWriteCmd(); ch == nil {
			if p.w.Buffered() == 0 {
				err = p.Error()
			} else {
				err = p.w.Flush()
			}
			if err == nil {
				if atomic.LoadInt32(&p.state) == 1 {
					ones[0], multi, ch = p.queue.WaitForWrite()
				} else {
					runtime.Gosched()
					continue
				}
			}
		}
		if ch != nil && multi == nil {
			multi = ones
		}
		for _, cmd := range multi {
			err = writeCmd(p.w, cmd.Commands())
		}
		if err != nil {
			if err != ErrClosing { // ignore ErrClosing to allow final QUIT command to be sent
				return
			}
			runtime.Gosched()
		}
	}
	return
}

func (p *pipe) _backgroundRead() (err error) {
	var (
		msg   RedisMessage
		cond  *sync.Cond
		ones  = make([]cmds.Completed, 1)
		multi []cmds.Completed
		ch    chan RedisResult
		ff    int // fulfilled count
		skip  int // skip rest push messages
		ver   = p.version
		pr    bool // push reply
	)

	defer func() {
		if err != nil && ff < len(multi) {
			for ; ff < len(multi); ff++ {
				ch <- newErrResult(err)
			}
			cond.L.Unlock()
			cond.Signal()
		}
	}()

	for {
		if msg, err = readNextMessage(p.r); err != nil {
			return
		}
		if msg.typ == '>' {
			if pr = p.handlePush(msg.values); !pr {
				continue
			}
			if skip > 0 {
				skip--
				pr = false
				continue
			}
		} else if ver < 7 && len(msg.values) != 0 {
			// This is a workaround for Redis 6's broken invalidation protocol: https://github.com/redis/redis/issues/8935
			// When Redis 6 handles MULTI, MGET, or other multi-keys command,
			// it will send invalidation message immediately if it finds the keys are expired, thus causing the multi-keys command response to be broken.
			// We fix this by fetching the next message and patch it back to the response.
			i := 0
			for j, v := range msg.values {
				if v.typ == '>' {
					p.handlePush(v.values)
				} else {
					if i != j {
						msg.values[i] = v
					}
					i++
				}
			}
			for ; i < len(msg.values); i++ {
				if msg.values[i], err = readNextMessage(p.r); err != nil {
					return
				}
			}
		}
		// if unfulfilled multi commands are lead by opt-in and get success response
		if ff == len(multi)-1 && multi[0].IsOptIn() && len(msg.values) >= 2 {
			if cacheable := cmds.Cacheable(multi[len(multi)-2]); cacheable.IsMGet() {
				cc := cacheable.MGetCacheCmd()
				for i, cp := range msg.values[len(msg.values)-1].values {
					cp.attrs = cacheMark
					p.cache.Update(cacheable.MGetCacheKey(i), cc, cp, msg.values[i].integer)
				}
			} else {
				for i := 1; i < len(msg.values); i += 2 {
					cacheable = cmds.Cacheable(multi[i+2])
					ck, cc := cacheable.CacheKey()
					cp := msg.values[i]
					cp.attrs = cacheMark
					p.cache.Update(ck, cc, cp, msg.values[i-1].integer)
				}
			}
		}
		if ff == len(multi) {
			ff = 0
			ones[0], multi, ch, cond = p.queue.NextResultCh() // ch should not be nil, otherwise it must be a protocol bug
			if ch == nil {
				cond.L.Unlock()
				// Redis will send sunsubscribe notification proactively in the event of slot migration.
				// We should ignore them and go fetch next message.
				if pr && msg.values[0].string == "sunsubscribe" {
					pr = false
					continue
				}
				panic(protocolbug)
			}
			if multi == nil {
				multi = ones
			}
		}
		if pr {
			pr = false
			// Redis will send sunsubscribe notification proactively in the event of slot migration.
			// We should ignore them and go fetch next message.
			if msg.values[0].string == "sunsubscribe" && (!multi[ff].NoReply() || multi[ff].Commands()[0] != "SUNSUBSCRIBE") {
				continue
			}
			if !multi[ff].NoReply() {
				panic(protocolbug)
			}
			if len(multi[ff].Commands()) == 1 { // wildcard unsubscribe
				switch multi[ff].Commands()[0] {
				case "UNSUBSCRIBE":
					skip = p.nsubs.Confirmed()
				case "PUNSUBSCRIBE":
					skip = p.psubs.Confirmed()
				case "SUNSUBSCRIBE":
					skip = p.ssubs.Confirmed()
				}
			} else {
				skip = len(multi[ff].Commands()) - 2
			}
			msg = RedisMessage{} // override successful subscribe/unsubscribe response to empty
		}
		ch <- newResult(msg, err)
		if ff++; ff == len(multi) {
			cond.L.Unlock()
			cond.Signal()
		}
	}
}

func (p *pipe) _backgroundPing(stop <-chan struct{}) (err error) {
	ticker := time.NewTicker(p.pinggap)
	defer ticker.Stop()
	for err == nil {
		select {
		case <-ticker.C:
			if atomic.LoadInt32(&p.blcksig) != 0 {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
			err = p.Do(ctx, cmds.PingCmd).NonRedisError()
			cancel()
			if err != nil && atomic.LoadInt32(&p.blcksig) != 0 {
				err = nil
			}
		case <-stop:
			return
		}
	}
	return err
}

func (p *pipe) handlePush(values []RedisMessage) (reply bool) {
	if len(values) < 2 {
		return
	}
	// TODO: handle other push data
	// tracking-redir-broken
	// server-cpu-usage
	switch values[0].string {
	case "invalidate":
		if p.cache != nil {
			if values[1].IsNil() {
				p.cache.Delete(nil)
			} else {
				p.cache.Delete(values[1].values)
			}
		}
		if p.onInvalidations != nil {
			if values[1].IsNil() {
				p.onInvalidations(nil)
			} else {
				p.onInvalidations(values[1].values)
			}
		}
	case "message":
		if len(values) >= 3 {
			m := PubSubMessage{Channel: values[1].string, Message: values[2].string}
			p.nsubs.Publish(values[1].string, m)
			p.pshks.Load().(*pshks).hooks.OnMessage(m)
		}
	case "pmessage":
		if len(values) >= 4 {
			m := PubSubMessage{Pattern: values[1].string, Channel: values[2].string, Message: values[3].string}
			p.psubs.Publish(values[1].string, m)
			p.pshks.Load().(*pshks).hooks.OnMessage(m)
		}
	case "smessage":
		if len(values) >= 3 {
			m := PubSubMessage{Channel: values[1].string, Message: values[2].string}
			p.ssubs.Publish(values[1].string, m)
			p.pshks.Load().(*pshks).hooks.OnMessage(m)
		}
	case "unsubscribe":
		p.nsubs.Unsubscribe(values[1].string)
		if len(values) >= 3 {
			p.pshks.Load().(*pshks).hooks.OnSubscription(PubSubSubscription{Kind: values[0].string, Channel: values[1].string, Count: values[2].integer})
		}
		return true
	case "punsubscribe":
		p.psubs.Unsubscribe(values[1].string)
		if len(values) >= 3 {
			p.pshks.Load().(*pshks).hooks.OnSubscription(PubSubSubscription{Kind: values[0].string, Channel: values[1].string, Count: values[2].integer})
		}
		return true
	case "sunsubscribe":
		p.ssubs.Unsubscribe(values[1].string)
		if len(values) >= 3 {
			p.pshks.Load().(*pshks).hooks.OnSubscription(PubSubSubscription{Kind: values[0].string, Channel: values[1].string, Count: values[2].integer})
		}
		return true
	case "subscribe":
		p.nsubs.Confirm(values[1].string)
		if len(values) >= 3 {
			p.pshks.Load().(*pshks).hooks.OnSubscription(PubSubSubscription{Kind: values[0].string, Channel: values[1].string, Count: values[2].integer})
		}
		return true
	case "psubscribe":
		p.psubs.Confirm(values[1].string)
		if len(values) >= 3 {
			p.pshks.Load().(*pshks).hooks.OnSubscription(PubSubSubscription{Kind: values[0].string, Channel: values[1].string, Count: values[2].integer})
		}
		return true
	case "ssubscribe":
		p.ssubs.Confirm(values[1].string)
		if len(values) >= 3 {
			p.pshks.Load().(*pshks).hooks.OnSubscription(PubSubSubscription{Kind: values[0].string, Channel: values[1].string, Count: values[2].integer})
		}
		return true
	}
	return false
}

func (p *pipe) Receive(ctx context.Context, subscribe cmds.Completed, fn func(message PubSubMessage)) error {
	if p.nsubs == nil || p.psubs == nil || p.ssubs == nil {
		return ErrClosing
	}

	var sb *subs
	cmd, args := subscribe.Commands()[0], subscribe.Commands()[1:]

	switch cmd {
	case "SUBSCRIBE":
		sb = p.nsubs
	case "PSUBSCRIBE":
		sb = p.psubs
	case "SSUBSCRIBE":
		sb = p.ssubs
	default:
		panic(wrongreceive)
	}
	if p.version < 6 {
		return ErrRESP2PubSub
	}

	if ch, cancel := sb.Subscribe(args); ch != nil {
		defer cancel()
		if err := p.Do(ctx, subscribe).Error(); err != nil {
			return err
		}
		if ctxCh := ctx.Done(); ctxCh == nil {
			for msg := range ch {
				fn(msg)
			}
		} else {
		next:
			select {
			case msg, ok := <-ch:
				if ok {
					fn(msg)
					goto next
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return p.Error()
}

func (p *pipe) CleanSubscriptions() {
	if atomic.LoadInt32(&p.state) == 1 {
		if p.version >= 7 {
			p.DoMulti(context.Background(), cmds.UnsubscribeCmd, cmds.PUnsubscribeCmd, cmds.SUnsubscribeCmd)
		} else {
			p.DoMulti(context.Background(), cmds.UnsubscribeCmd, cmds.PUnsubscribeCmd)
		}
	}
}

func (p *pipe) SetPubSubHooks(hooks PubSubHooks) <-chan error {
	if hooks.isZero() {
		if old := p.pshks.Swap(emptypshks).(*pshks); old.close != nil {
			close(old.close)
		}
		return nil
	}
	if hooks.OnMessage == nil {
		hooks.OnMessage = func(m PubSubMessage) {}
	}
	if hooks.OnSubscription == nil {
		hooks.OnSubscription = func(s PubSubSubscription) {}
	}
	ch := make(chan error, 1)
	if old := p.pshks.Swap(&pshks{hooks: hooks, close: ch}).(*pshks); old.close != nil {
		close(old.close)
	}
	if err := p.Error(); err != nil {
		if old := p.pshks.Swap(emptypshks).(*pshks); old.close != nil {
			old.close <- err
			close(old.close)
		}
	}
	if atomic.AddInt32(&p.waits, 1) == 1 && atomic.LoadInt32(&p.state) == 0 {
		p.background()
	}
	atomic.AddInt32(&p.waits, -1)
	return ch
}

func (p *pipe) Info() map[string]RedisMessage {
	return p.info
}

func (p *pipe) Do(ctx context.Context, cmd cmds.Completed) (resp RedisResult) {
	if err := ctx.Err(); err != nil {
		return newErrResult(ctx.Err())
	}

	if cmd.IsBlock() {
		atomic.AddInt32(&p.blcksig, 1)
		defer func() {
			if resp.err == nil {
				atomic.AddInt32(&p.blcksig, -1)
			}
		}()
	}

	waits := atomic.AddInt32(&p.waits, 1) // if this is 1, and background worker is not started, no need to queue
	state := atomic.LoadInt32(&p.state)

	if state == 1 {
		goto queue
	}

	if state == 0 {
		if waits != 1 {
			goto queue
		}
		if cmd.NoReply() {
			if p.version < 6 {
				atomic.AddInt32(&p.waits, -1)
				return newErrResult(ErrRESP2PubSub)
			}
			p.background()
			goto queue
		}
		dl, ok := ctx.Deadline()
		if !ok && ctx.Done() != nil {
			p.background()
			goto queue
		}
		resp = p.syncDo(dl, ok, cmd)
	} else {
		resp = newErrResult(p.Error())
	}
	if left := atomic.AddInt32(&p.waits, -1); state == 0 && waits == 1 && left != 0 {
		p.background()
	}
	return resp

queue:
	ch := p.queue.PutOne(cmd)
	if ctxCh := ctx.Done(); ctxCh == nil {
		resp = <-ch
		atomic.AddInt32(&p.waits, -1)
	} else {
		select {
		case resp = <-ch:
			atomic.AddInt32(&p.waits, -1)
		case <-ctxCh:
			resp = newErrResult(ctx.Err())
			go func() {
				<-ch
				atomic.AddInt32(&p.waits, -1)
			}()
		}
	}
	return resp
}

func (p *pipe) DoMulti(ctx context.Context, multi ...cmds.Completed) []RedisResult {
	resp := make([]RedisResult, len(multi))
	if err := ctx.Err(); err != nil {
		for i := 0; i < len(resp); i++ {
			resp[i] = newErrResult(err)
		}
		return resp
	}

	isOptIn := multi[0].IsOptIn() // len(multi) > 0 should have already been checked by upper layer
	noReply := false
	isBlock := false

	for _, cmd := range multi {
		if cmd.NoReply() {
			if p.version < 6 {
				for i := 0; i < len(resp); i++ {
					resp[i] = newErrResult(ErrRESP2PubSub)
				}
				return resp
			}
			noReply = true
			break
		}
	}

	for _, cmd := range multi {
		if cmd.IsBlock() {
			isBlock = true
			break
		}
	}

	if isBlock {
		atomic.AddInt32(&p.blcksig, 1)
		defer func() {
			for _, r := range resp {
				if r.err != nil {
					return
				}
			}
			atomic.AddInt32(&p.blcksig, -1)
		}()
	}

	waits := atomic.AddInt32(&p.waits, 1) // if this is 1, and background worker is not started, no need to queue
	state := atomic.LoadInt32(&p.state)

	if state == 1 {
		goto queue
	}

	if state == 0 {
		if waits != 1 {
			goto queue
		}
		if isOptIn || noReply {
			p.background()
			goto queue
		}
		dl, ok := ctx.Deadline()
		if !ok && ctx.Done() != nil {
			p.background()
			goto queue
		}
		resp = p.syncDoMulti(dl, ok, resp, multi)
	} else {
		err := newErrResult(p.Error())
		for i := 0; i < len(resp); i++ {
			resp[i] = err
		}
	}
	if left := atomic.AddInt32(&p.waits, -1); state == 0 && waits == 1 && left != 0 {
		p.background()
	}
	return resp

queue:
	ch := p.queue.PutMulti(multi)
	var i int
	if ctxCh := ctx.Done(); ctxCh == nil {
		for ; i < len(resp); i++ {
			resp[i] = <-ch
		}
	} else {
		for ; i < len(resp); i++ {
			select {
			case resp[i] = <-ch:
			case <-ctxCh:
				goto abort
			}
		}
	}
	atomic.AddInt32(&p.waits, -1)
	return resp
abort:
	go func(i int) {
		for ; i < len(resp); i++ {
			<-ch
		}
		atomic.AddInt32(&p.waits, -1)
	}(i)
	err := newErrResult(ctx.Err())
	for ; i < len(resp); i++ {
		resp[i] = err
	}
	return resp
}

func (p *pipe) syncDo(dl time.Time, dlOk bool, cmd cmds.Completed) (resp RedisResult) {
	if dlOk {
		p.conn.SetDeadline(dl)
		defer p.conn.SetDeadline(time.Time{})
	} else if p.timeout > 0 && !cmd.IsBlock() {
		p.conn.SetDeadline(time.Now().Add(p.timeout))
		defer p.conn.SetDeadline(time.Time{})
	}

	var msg RedisMessage
	err := writeCmd(p.w, cmd.Commands())
	if err == nil {
		if err = p.w.Flush(); err == nil {
			msg, err = syncRead(p.r)
		}
	}
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			err = context.DeadlineExceeded
		}
		p.error.CompareAndSwap(nil, &errs{error: err})
		atomic.CompareAndSwapInt32(&p.state, 1, 3) // stopping the worker and let it do the cleaning
		p.background()                             // start the background worker
	}
	return newResult(msg, err)
}

func (p *pipe) syncDoMulti(dl time.Time, dlOk bool, resp []RedisResult, multi []cmds.Completed) []RedisResult {
	if dlOk {
		p.conn.SetDeadline(dl)
		defer p.conn.SetDeadline(time.Time{})
	} else if p.timeout > 0 {
		for _, cmd := range multi {
			if cmd.IsBlock() {
				goto process
			}
		}
		p.conn.SetDeadline(time.Now().Add(p.timeout))
		defer p.conn.SetDeadline(time.Time{})
	}
process:
	var err error
	var msg RedisMessage

	for _, cmd := range multi {
		_ = writeCmd(p.w, cmd.Commands())
	}
	if err = p.w.Flush(); err != nil {
		goto abort
	}
	for i := 0; i < len(resp); i++ {
		if msg, err = syncRead(p.r); err != nil {
			goto abort
		}
		resp[i] = newResult(msg, err)
	}
	return resp
abort:
	if errors.Is(err, os.ErrDeadlineExceeded) {
		err = context.DeadlineExceeded
	}
	p.error.CompareAndSwap(nil, &errs{error: err})
	atomic.CompareAndSwapInt32(&p.state, 1, 3) // stopping the worker and let it do the cleaning
	p.background()                             // start the background worker
	for i := 0; i < len(resp); i++ {
		resp[i] = newErrResult(err)
	}
	return resp
}

func syncRead(r *bufio.Reader) (m RedisMessage, err error) {
next:
	if m, err = readNextMessage(r); err != nil {
		return m, err
	}
	if m.typ == '>' {
		goto next
	}
	return m, nil
}

func (p *pipe) DoCache(ctx context.Context, cmd cmds.Cacheable, ttl time.Duration) RedisResult {
	if p.cache == nil {
		return p.Do(ctx, cmds.Completed(cmd))
	}
	if cmd.IsMGet() {
		return p.doCacheMGet(ctx, cmd, ttl)
	}
	ck, cc := cmd.CacheKey()
	if v, entry := p.cache.GetOrPrepare(ck, cc, ttl); v.typ != 0 {
		return newResult(v, nil)
	} else if entry != nil {
		return newResult(entry.Wait())
	}
	resp := p.DoMulti(
		ctx,
		cmds.OptInCmd,
		cmds.MultiCmd,
		cmds.NewCompleted([]string{"PTTL", ck}),
		cmds.Completed(cmd),
		cmds.ExecCmd,
	)
	exec, err := resp[4].ToArray()
	if err != nil {
		if _, ok := err.(*RedisError); !ok {
			p.cache.Cancel(ck, cc, RedisMessage{}, err)
			return newErrResult(err)
		}
		// EXEC aborted, return err of the input cmd in MULTI block
		if resp[3].val.typ != '+' {
			p.cache.Cancel(ck, cc, resp[3].val, nil)
			return newResult(resp[3].val, nil)
		}
		p.cache.Cancel(ck, cc, resp[4].val, nil)
		return newResult(resp[4].val, nil)
	}
	return newResult(exec[1], nil)
}

func (p *pipe) doCacheMGet(ctx context.Context, cmd cmds.Cacheable, ttl time.Duration) RedisResult {
	commands := cmd.Commands()
	entries := make(map[int]*entry)
	builder := cmds.NewBuilder(cmds.InitSlot)
	result := RedisResult{val: RedisMessage{typ: '*', values: nil}}
	mgetcc := cmd.MGetCacheCmd()
	keys := len(commands) - 1
	if mgetcc[0] == 'J' {
		keys-- // the last one of JSON.MGET is a path, not a key
	}
	var rewrite cmds.Arbitrary
	for i, key := range commands[1 : keys+1] {
		v, entry := p.cache.GetOrPrepare(key, mgetcc, ttl)
		if v.typ != 0 { // cache hit for one key
			if len(result.val.values) == 0 {
				result.val.values = make([]RedisMessage, keys)
			}
			result.val.values[i] = v
			continue
		}
		if entry != nil {
			entries[i] = entry // store entries for later entry.Wait() to avoid MGET deadlock each others.
			continue
		}
		if rewrite.IsZero() {
			rewrite = builder.Arbitrary(commands[0])
		}
		rewrite = rewrite.Args(key)
	}

	var partial []RedisMessage
	if !rewrite.IsZero() {
		var rewritten cmds.Completed
		var keys int
		if mgetcc[0] == 'J' { // rewrite JSON.MGET path
			rewritten = rewrite.Args(commands[len(commands)-1]).MultiGet()
			keys = len(rewritten.Commands()) - 2
		} else {
			rewritten = rewrite.MultiGet()
			keys = len(rewritten.Commands()) - 1
		}

		multi := make([]cmds.Completed, 0, keys+4)
		multi = append(multi, cmds.OptInCmd, cmds.MultiCmd)
		for _, key := range rewritten.Commands()[1 : keys+1] {
			multi = append(multi, builder.Pttl().Key(key).Build())
		}
		multi = append(multi, rewritten, cmds.ExecCmd)

		resp := p.DoMulti(ctx, multi...)
		exec, err := resp[len(multi)-1].ToArray()
		if err != nil {
			var msg RedisMessage
			if _, ok := err.(*RedisError); ok {
				err = nil
				if resp[len(multi)-2].val.typ != '+' { // EXEC aborted, return err of the input cmd in MULTI block
					msg = resp[len(multi)-2].val
				} else {
					msg = resp[len(multi)-1].val
				}
			}
			for _, key := range rewritten.Commands()[1 : keys+1] {
				p.cache.Cancel(key, mgetcc, msg, err)
			}
			return newResult(msg, err)
		}
		defer func() {
			for _, cmd := range multi[2 : len(multi)-1] {
				cmds.Put(cmd.CommandSlice())
			}
		}()
		if len(rewritten.Commands()) == len(commands) { // all cache miss
			return newResult(exec[len(exec)-1], nil)
		}
		partial = exec[len(exec)-1].values
	} else { // all cache hit
		result.val.attrs = cacheMark
	}

	if len(result.val.values) == 0 {
		result.val.values = make([]RedisMessage, keys)
	}
	for i, entry := range entries {
		v, err := entry.Wait()
		if err != nil {
			return newErrResult(err)
		}
		result.val.values[i] = v
	}

	j := 0
	for _, ret := range partial {
		for ; j < len(result.val.values); j++ {
			if result.val.values[j].typ == 0 {
				result.val.values[j] = ret
				break
			}
		}
	}
	return result
}

func (p *pipe) DoMultiCache(ctx context.Context, multi ...CacheableTTL) []RedisResult {
	if p.cache == nil {
		commands := make([]cmds.Completed, len(multi))
		for i, ct := range multi {
			commands[i] = cmds.Completed(ct.Cmd)
		}
		return p.DoMulti(ctx, commands...)
	}
	results := make([]RedisResult, len(multi))
	entries := make(map[int]*entry)
	missing := []cmds.Completed{cmds.OptInCmd, cmds.MultiCmd}
	for i, ct := range multi {
		if ct.Cmd.IsMGet() {
			panic(panicmgetcsc)
		}
		ck, cc := ct.Cmd.CacheKey()
		v, entry := p.cache.GetOrPrepare(ck, cc, ct.TTL)
		if v.typ != 0 { // cache hit for one key
			results[i] = newResult(v, nil)
			continue
		}
		if entry != nil {
			entries[i] = entry // store entries for later entry.Wait() to avoid MGET deadlock each others.
			continue
		}
		missing = append(missing, cmds.NewCompleted([]string{"PTTL", ck}), cmds.Completed(ct.Cmd))
	}

	var exec []RedisMessage
	var err error
	if len(missing) > 2 {
		missing = append(missing, cmds.ExecCmd)
		resp := p.DoMulti(ctx, missing...)
		exec, err = resp[len(missing)-1].ToArray()
		if err != nil {
			var msg RedisMessage
			if _, ok := err.(*RedisError); ok {
				for i := 1; i < len(resp); i += 2 { // EXEC aborted, return the first err of the input cmd in MULTI block
					if resp[i].val.typ == '-' || resp[i].val.typ == '_' || resp[i].val.typ == '!' {
						msg = resp[i].val
						err = nil
						break
					}
				}
			}
			for i := 3; i < len(missing); i += 2 {
				cacheable := cmds.Cacheable(missing[i])
				ck, cc := cacheable.CacheKey()
				p.cache.Cancel(ck, cc, msg, err)
			}
			for i := range results {
				results[i] = newResult(msg, err)
			}
			return results
		}
	}

	for i, entry := range entries {
		results[i] = newResult(entry.Wait())
	}

	j := 0
	for i := 1; i < len(exec); i += 2 {
		for ; j < len(results); j++ {
			if results[j].val.typ == 0 && results[j].err == nil {
				results[j] = newResult(exec[i], nil)
				break
			}
		}
	}
	return results
}

func (p *pipe) Error() error {
	if err, ok := p.error.Load().(*errs); ok {
		return err.error
	}
	return nil
}

func (p *pipe) Close() {
	p.error.CompareAndSwap(nil, errClosing)
	block := atomic.AddInt32(&p.blcksig, 1)
	waits := atomic.AddInt32(&p.waits, 1)
	stopping1 := atomic.CompareAndSwapInt32(&p.state, 0, 2)
	stopping2 := atomic.CompareAndSwapInt32(&p.state, 1, 2)
	if p.queue != nil {
		if stopping1 && waits == 1 { // make sure there is no sync read
			p.background()
		}
		if block == 1 && (stopping1 || stopping2) { // make sure there is no block cmd
			<-p.queue.PutOne(cmds.QuitCmd)
		}
	}
	atomic.AddInt32(&p.waits, -1)
	atomic.AddInt32(&p.blcksig, -1)
	if p.conn != nil {
		p.conn.Close()
	}
}

type pshks struct {
	hooks PubSubHooks
	close chan error
}

var emptypshks = &pshks{
	hooks: PubSubHooks{
		OnMessage:      func(m PubSubMessage) {},
		OnSubscription: func(s PubSubSubscription) {},
	},
	close: nil,
}

func deadFn() *pipe {
	dead := &pipe{state: 3}
	dead.error.Store(errClosing)
	dead.pshks.Store(emptypshks)
	return dead
}

const (
	protocolbug  = "protocol bug, message handled out of order"
	wrongreceive = "only SUBSCRIBE, SSUBSCRIBE, or PSUBSCRIBE command are allowed in Receive"
	panicmgetcsc = "MGET and JSON.MGET in DoMultiCache are not implemented, use DoCache instead"
)

var cacheMark = &(RedisMessage{})
var errClosing = &errs{error: ErrClosing}

type errs struct{ error }
