// This file contains the implementation for the HTTP request queue used by the
// KEDA external scaler implementation
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/kedacore/http-add-on/pkg/k8s"
	"github.com/kedacore/http-add-on/pkg/queue"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type queuePinger struct {
	k8sCl        *kubernetes.Clientset
	ns           string
	svcName      string
	adminPort    string
	pingMut      *sync.RWMutex
	lastPingTime time.Time
	allCounts    map[string]int
	lggr         logr.Logger
}

func newQueuePinger(
	ctx context.Context,
	lggr logr.Logger,
	k8sCl *kubernetes.Clientset,
	ns,
	svcName,
	adminPort string,
	pingTicker *time.Ticker,
) *queuePinger {
	pingMut := new(sync.RWMutex)
	pinger := &queuePinger{
		k8sCl:     k8sCl,
		ns:        ns,
		svcName:   svcName,
		adminPort: adminPort,
		pingMut:   pingMut,
		lggr:      lggr,
	}

	go func() {
		defer pingTicker.Stop()
		for range pingTicker.C {
			if err := pinger.requestCounts(ctx); err != nil {
				lggr.Error(err, "getting request counts")
			}
		}
	}()

	return pinger
}

func (q *queuePinger) counts() map[string]int {
	q.pingMut.RLock()
	defer q.pingMut.RUnlock()
	return q.allCounts
}

func (q *queuePinger) requestCounts(ctx context.Context) error {
	lggr := q.lggr.WithName("queuePinger.requestCounts")
	endpointsCl := q.k8sCl.CoreV1().Endpoints(q.ns)
	endpoints, err := endpointsCl.Get(ctx, q.svcName, metav1.GetOptions{})
	if err != nil {
		lggr.Error(err, "getting endpoints for service", "serviceName", q.svcName)
		return err
	}

	endpointURLs, err := k8s.EndpointsForService(
		ctx,
		endpoints,
		q.svcName,
		q.adminPort,
	)
	if err != nil {
		return err
	}

	countsCh := make(chan *queue.Counts)
	var wg sync.WaitGroup

	for _, endpoint := range endpointURLs {
		wg.Add(1)
		go func(u *url.URL) {
			defer wg.Done()
			addr := fmt.Sprintf(
				"%s%s",
				u.String(),
				queue.CountsPath,
			)
			counts, err := queue.GetCounts(
				ctx,
				lggr,
				http.DefaultClient,
				addr,
			)
			if err != nil {
				lggr.Error(
					err,
					"getting queue counts from interceptor",
					"interceptorAddress",
					addr,
				)
				return
			}
			countsCh <- counts
		}(endpoint)
	}

	go func() {
		wg.Wait()
		close(countsCh)
	}()

	totalCounts := make(map[string]int)
	for count := range countsCh {
		for host, val := range count.Counts {
			totalCounts[host] += val
		}
	}

	q.pingMut.Lock()
	defer q.pingMut.Unlock()
	q.allCounts = totalCounts
	q.lastPingTime = time.Now()

	return nil

}
