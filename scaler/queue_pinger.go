// This file contains the implementation for the HTTP request queue used by the
// KEDA external scaler implementation
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

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
}

func newQueuePinger(
	ctx context.Context,
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
	}

	go func() {
		defer pingTicker.Stop()
		for range pingTicker.C {
			if err := pinger.requestCounts(ctx); err != nil {
				log.Printf("Error getting request counts (%s)", err)
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
	log.Printf("queuePinger.requestCounts")
	endpointsCl := q.k8sCl.CoreV1().Endpoints(q.ns)
	endpoints, err := endpointsCl.Get(ctx, q.svcName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	queueSizeCh := make(chan map[string]int)
	var wg sync.WaitGroup

	for _, subset := range endpoints.Subsets {
		for _, addr := range subset.Addresses {
			wg.Add(1)
			go func(addr string) {
				defer wg.Done()
				completeAddr := fmt.Sprintf("http://%s:%s/queue", addr, q.adminPort)
				resp, err := http.Get(completeAddr)
				if err != nil {
					log.Printf("Error in pinger with GET %s (%s)", completeAddr, err)
					return
				}
				defer resp.Body.Close()
				respData := map[string]int{}
				if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
					log.Printf("Error decoding request to %s (%s)", completeAddr, err)
					return
				}
				log.Printf("\n--\ncurSize for address %s: %v\n--\n", addr, respData)
				queueSizeCh <- respData
				log.Printf("Sent curSize %v for address %s", respData, addr)
			}(addr.IP)
		}
	}

	go func() {
		wg.Wait()
		close(queueSizeCh)
	}()

	totalCounts := make(map[string]int)
	for count := range queueSizeCh {
		for host, val := range count {
			totalCounts[host] += val
		}
	}

	q.pingMut.Lock()
	defer q.pingMut.Unlock()
	q.allCounts = totalCounts
	q.lastPingTime = time.Now()
	log.Printf("Finished getting aggregate current sizes %v", q.allCounts)

	return nil

}
