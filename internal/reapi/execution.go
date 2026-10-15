package reapi

// Execution messages follow remote_execution.proto and google/longrunning/operations.proto.
import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ExecutionError carries an executor infrastructure status through the scheduler.
type ExecutionError struct {
	Code    int
	Message string
	Missing []byte
}

func (e *ExecutionError) Error() string { return e.Message }

const executionType = "type.googleapis.com/build.bazel.remote.execution.v2."

type operation struct {
	name      string
	digest    []byte
	stage     int64
	response  []byte
	changed   chan struct{}
	completed time.Time
}

// Execution retains operations until shutdown. Work is independent of RPC lifetimes.
// Run must return an ActionResult or an infrastructure error; nonzero command exit
// codes belong in ActionResult, not in the ExecuteResponse status.
type Execution struct {
	externalScheduler bool
	Store             *Store
	Run               func(context.Context, []byte, []byte) ([]byte, error)
	mu                sync.Mutex
	ops               map[string]*operation
	queue             []*operation
	wake              chan struct{}
	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	next              uint64
}

// NewExecution uses slots workers, or delegates all queueing to Run when slots < 0.
func NewExecution(store *Store, slots int, run func(context.Context, []byte, []byte) ([]byte, error)) *Execution {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Execution{externalScheduler: slots < 0, Store: store, Run: run, ops: map[string]*operation{}, wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel}
	for i := 0; i < slots; i++ {
		e.wg.Add(1)
		go e.worker()
	}
	return e
}
func (e *Execution) Close() { e.mu.Lock(); e.cancel(); e.mu.Unlock(); e.wg.Wait() }
func anyMessage(typ string, b []byte) []byte {
	return msg(bytesField(1, []byte(typ)), bytesField(2, b))
}
func (o *operation) marshal() []byte {
	b := msg(bytesField(1, []byte(o.name)), bytesField(2, anyMessage(executionType+"ExecuteOperationMetadata", msg(intField(1, o.stage), bytesField(2, o.digest)))))
	if o.response != nil {
		b = append(b, msg(intField(3, 1), bytesField(5, anyMessage(executionType+"ExecuteResponse", o.response)))...)
	}
	return b
}
func (e *Execution) update(o *operation, stage int64, response []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	o.stage, o.response = stage, response
	if response != nil {
		o.completed = time.Now()
	}
	close(o.changed)
	o.changed = make(chan struct{})
}
func validatePlatform(p []byte) error {
	for _, v := range all(p, 1) {
		k, val := string(get(v, 1).b), string(get(v, 2).b)
		switch k {
		case "OSFamily":
			if val != "Linux" && val != "linux" {
				return fmt.Errorf("unsupported platform %s=%s", k, val)
			}
		case "container-image":
			if val != "" {
				return fmt.Errorf("unsupported platform %s=%s", k, val)
			}
		default:
			return fmt.Errorf("unsupported platform %s=%s", k, val)
		}
	}
	return nil
}
func (e *Execution) start(b []byte) (*operation, int, error) {
	if f := get(b, 9).v; f != 0 && f != 1 {
		return nil, 3, fmt.Errorf("only SHA256 execution is supported")
	}
	d := get(b, 6).b
	if _, err := key(d); err != nil {
		return nil, 3, err
	}
	if get(d, 2).v > maxMessage {
		return nil, 8, capacityError()
	}
	action, err := e.Store.read("cas-", d)
	if err != nil {
		return nil, 9, fmt.Errorf("missing Action: %w", err)
	}
	if _, err = decode(action); err != nil {
		return nil, 3, err
	}
	if get(get(action, 1).b, 2).v > maxMessage {
		return nil, 8, capacityError()
	}
	command, err := e.Store.read("cas-", get(action, 1).b)
	if err != nil {
		return nil, 9, fmt.Errorf("missing Command: %w", err)
	}
	if _, err = decode(command); err != nil {
		return nil, 3, err
	}
	if _, err = actionTimeout(action); err != nil {
		return nil, 3, err
	}
	if err = validatePlatform(get(action, 10).b); err != nil {
		return nil, 9, err
	}
	if err = validatePlatform(get(command, 5).b); err != nil {
		return nil, 9, err
	}
	var cached []byte
	if get(b, 3).v == 0 {
		if result, err := e.Store.read("ac-", d); err == nil && e.Store.actionAvailable(result) {
			cached = result
		}
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, 13, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for name, op := range e.ops {
		if !op.completed.IsZero() && time.Since(op.completed) > time.Hour {
			delete(e.ops, name)
		}
	}
	if e.ctx.Err() != nil {
		return nil, 14, fmt.Errorf("executor stopped")
	}
	o := &operation{name: "operations/" + hex.EncodeToString(id[:]), digest: d, stage: 2, changed: make(chan struct{})}
	e.ops[o.name] = o
	if cached != nil {
		o.stage, o.response, o.completed = 4, msg(bytesField(1, cached), intField(2, 1)), time.Now()
		return o, 0, nil
	}
	if e.externalScheduler {
		e.wg.Add(1)
		go func() { defer e.wg.Done(); e.execute(o) }()
		return o, 0, nil
	}
	e.queue = append(e.queue, o)
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return o, 0, nil
}
func (e *Execution) worker() {
	defer e.wg.Done()
	for {
		if e.ctx.Err() != nil {
			return
		}
		e.mu.Lock()
		var o *operation
		if len(e.queue) > 0 {
			o = e.queue[0]
			e.queue = e.queue[1:]
			select {
			case e.wake <- struct{}{}:
			default:
			}
		}
		e.mu.Unlock()
		if o == nil {
			select {
			case <-e.ctx.Done():
				return
			case <-e.wake:
				continue
			}
		}
		e.execute(o)
	}
}

func (e *Execution) execute(o *operation) {
	var unsafe atomic.Bool
	ctx := context.WithValue(e.ctx, unsafeKey{}, &unsafe)
	ctx = context.WithValue(ctx, stageKey{}, func() { e.update(o, 3, nil) })
	if !e.externalScheduler {
		MarkExecuting(ctx)
	}
	action, err := e.Store.read("cas-", o.digest)
	var result []byte
	if err == nil {
		var command []byte
		command, err = e.Store.read("cas-", get(action, 1).b)
		if err == nil {
			result, err = e.Run(ctx, action, command)
		}
	}
	response := bytesField(1, result)
	if err != nil {
		code := int64(13)
		var status *ExecutionError
		if errors.As(err, &status) && status.Code != 0 {
			code = int64(status.Code)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			code = 4
		} else if errors.Is(err, context.Canceled) {
			code = 1
		}
		st := msg(intField(1, code), bytesField(2, []byte(err.Error())))
		if status != nil && status.Missing != nil {
			d := status.Missing
			violation := msg(bytesField(1, []byte("MISSING")), bytesField(2, []byte(fmt.Sprintf("blobs/%s/%d", get(d, 1).b, get(d, 2).v))))
			st = append(st, bytesField(3, anyMessage("type.googleapis.com/google.rpc.PreconditionFailure", bytesField(1, violation)))...)
		}
		response = bytesField(3, st)
	} else if !unsafe.Load() && get(action, 7).v == 0 && get(result, 4).v == 0 {
		if err = e.Store.put("ac-", o.digest, result); err != nil {
			response = msg(bytesField(1, result), bytesField(5, []byte("cache write: "+err.Error())))
		}
	}
	e.update(o, 4, response)
}

type stageKey struct{}
type unsafeKey struct{}

func DisableActionCache(ctx context.Context) {
	if v, ok := ctx.Value(unsafeKey{}).(*atomic.Bool); ok {
		v.Store(true)
	}
}

// MarkExecuting records the scheduler commit point for operation metadata.
func MarkExecuting(ctx context.Context) {
	if f, ok := ctx.Value(stageKey{}).(func()); ok {
		f()
	}
}

func (e *Execution) stream(ctx context.Context, method string, b []byte, write func([]byte) error) (int, error) {
	var o *operation
	if strings.HasSuffix(method, "/Execute") {
		var code int
		var err error
		o, code, err = e.start(b)
		if err != nil {
			return code, err
		}
	} else {
		e.mu.Lock()
		o = e.ops[string(get(b, 1).b)]
		e.mu.Unlock()
		if o == nil {
			return 5, os.ErrNotExist
		}
	}
	for {
		e.mu.Lock()
		data, done, ch := o.marshal(), o.response != nil, o.changed
		e.mu.Unlock()
		if err := write(data); err != nil {
			return 13, err
		}
		if done {
			return 0, nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return 4, ctx.Err()
			}
			return 1, ctx.Err()
		case <-e.ctx.Done():
			return 14, fmt.Errorf("executor stopped")
		case <-ch:
		case <-time.After(30 * time.Second):
		}
	}
}

// safeRelative permits the empty working_directory but never an absolute or escaping path.
func safeRelative(p string) error {
	if strings.ContainsRune(p, 0) || filepath.IsAbs(p) || strings.Contains("/"+p+"/", "/../") {
		return fmt.Errorf("unsafe execution path %q", p)
	}
	return nil
}
func actionTimeout(action []byte) (time.Duration, error) {
	d := get(action, 6).b
	if d == nil || (get(d, 1).v == 0 && get(d, 2).v == 0) {
		return 30 * time.Minute, nil
	}
	sec, nano := get(d, 1).v, get(d, 2).v
	if sec > 86400 || nano >= 1e9 {
		return 0, fmt.Errorf("invalid action timeout (maximum 24h)")
	}
	return time.Duration(sec)*time.Second + time.Duration(nano), nil
}
