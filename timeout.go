package timeout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/vearne/gin-timeout/buffpool"
)

var defaultOptions TimeoutOptions

func init() {
	defaultOptions = TimeoutOptions{
		CallBack:        nil,
		DefaultMsg:      nil,
		Timeout:         3 * time.Second,
		TimeoutHttpCode: http.StatusServiceUnavailable,
		DefaultHttpCode: http.StatusOK,
	}
}

func Timeout(opts ...Option) gin.HandlerFunc {
	return func(c *gin.Context) {
		// **Notice**
		// because gin use sync.pool to reuse context object.
		// So this has to be used when the context has to be passed to a goroutine.
		cp := *c //nolint: govet
		c.Abort()

		// sync.Pool
		buffer := buffpool.GetBuff()
		tw := &TimeoutWriter{
			body: buffer, ResponseWriter: cp.Writer,
			h: make(http.Header),
		}
		tw.TimeoutOptions = defaultOptions

		// Loop through each option
		for _, opt := range opts {
			// Call the option giving the instantiated
			opt(tw)
		}

		cp.Writer = tw

		// wrap the request context with a timeout
		ctx, cancel := context.WithTimeout(cp.Request.Context(), tw.Timeout)
		defer cancel()

		cp.Request = cp.Request.WithContext(ctx)

		// Channel capacity must be greater than 0.
		// Otherwise, if the parent coroutine quit due to timeout,
		// the child coroutine may never be able to quit.
		finish := make(chan struct{}, 1)
		panicChan := make(chan interface{}, 1)
		go func() {
			defer func() {
				if p := recover(); p != nil {
					err := fmt.Errorf("gin-timeout recover:%v, stack: \n :%v", p, string(debug.Stack()))
					panicChan <- err
				}
			}()
			cp.Next()
			finish <- struct{}{}
		}()

		var err error
		var n int
		select {
		case p := <-panicChan:
			panic(p)

		case <-ctx.Done():
			tw.mu.Lock()
			defer tw.mu.Unlock()

			c.Error(fmt.Errorf("the timeout middleware saw error: %w", ctx.Err()))

			// Did it timeout?
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				tw.timedOut = true
				tw.ResponseWriter.WriteHeader(tw.TimeoutHttpCode)
			} else {
				tw.ResponseWriter.WriteHeader(tw.DefaultHttpCode)
			}

			if tw.DefaultMsg != nil {
				n, err = tw.ResponseWriter.Write(encodeBytes(tw.DefaultMsg))
				if err != nil {
					panic(err)
				}
				tw.size += n
			}

			cp.Abort()

			// execute callback func
			if tw.CallBack != nil {
				tw.CallBack(cp.Request)
			}
			// If timeout happen, the buffer cannot be cleared actively,
			// but wait for the GC to recycle.
		case <-finish:
			tw.mu.Lock()
			defer tw.mu.Unlock()
			dst := tw.ResponseWriter.Header()
			for k, vv := range tw.Header() {
				dst[k] = vv
			}

			if !tw.wroteHeader {
				tw.code = c.Writer.Status()
			}

			tw.ResponseWriter.WriteHeader(tw.code)
			if b := buffer.Bytes(); len(b) > 0 {
				if _, err = tw.ResponseWriter.Write(b); err != nil {
					panic(err)
				}
			}
			buffpool.PutBuff(buffer)
		}
	}
}

func encodeBytes(any interface{}) []byte {
	var resp []byte
	switch demsg := any.(type) {
	case string:
		resp = []byte(demsg)
	case []byte:
		resp = demsg
	default:
		resp, _ = json.Marshal(any)
	}
	return resp
}
