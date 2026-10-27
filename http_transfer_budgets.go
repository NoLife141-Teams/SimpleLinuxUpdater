package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type httpResourceBudgets struct{ backup, postcheck time.Duration }

var defaultHTTPResourceBudgets = httpResourceBudgets{backup: 30 * time.Minute, postcheck: applicationPostcheckTestTimeout + time.Minute}

type httpTransportBudgetKey struct{}
type httpTransportBudget struct {
	controller *http.ResponseController
	budgets    httpResourceBudgets
}

// Composition exposes transport controls without changing ordinary deadlines.
// Direct handler tests omit this transport layer; production and network tests
// use it with a real net/http writer. Unsupported controllers fail explicitly.
func applicationHTTPBudgetHandler(next http.Handler, budgets httpResourceBudgets) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		transport := httpTransportBudget{http.NewResponseController(w), budgets}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), httpTransportBudgetKey{}, transport)))
	})
}

// Called by an admitted handler, immediately before the long operation. Context
// expiry bounds processing too; cancellation interrupts blocked transport I/O.
func beginHTTPTransferBudget(c *gin.Context, upload, postcheck bool) (func(), bool) {
	ctx, finish, err := beginHTTPTransferBudgetContext(c.Request.Context(), upload, postcheck)
	if err != nil {
		failHTTPTransferBudget(c, err)
		return nil, false
	}
	c.Request = c.Request.WithContext(ctx)
	return finish, true
}

func beginHTTPTransferBudgetContext(parent context.Context, upload, postcheck bool) (context.Context, func(), error) {
	transport, ok := parent.Value(httpTransportBudgetKey{}).(httpTransportBudget)
	if !ok {
		return parent, func() {}, nil
	}
	budget := transport.budgets.backup
	if postcheck {
		budget = transport.budgets.postcheck
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	deadline, _ := ctx.Deadline()
	if upload {
		if err := transport.controller.SetReadDeadline(deadline); err != nil {
			cancel()
			return parent, nil, err
		}
	}
	if err := transport.controller.SetWriteDeadline(deadline); err != nil {
		cancel()
		return parent, nil, err
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(stopped)
		if upload {
			if err := transport.controller.SetReadDeadline(time.Now()); err != nil {
				log.Printf("Failed to interrupt backup upload: %v", err)
			}
		}
		if err := transport.controller.SetWriteDeadline(time.Now()); err != nil {
			log.Printf("Failed to interrupt HTTP transfer: %v", err)
		}
	})
	return ctx, func() {
		if !stop() {
			<-stopped
		}
		cancel()
	}, nil
}

func failHTTPTransferBudget(c *gin.Context, err error) {
	log.Printf("Failed to set admitted HTTP transfer deadline: %v", err)
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "HTTP transfer unavailable"})
}
