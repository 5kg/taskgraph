package etcdutil

import (
	"math"
	"time"

	"github.com/coreos/go-etcd/etcd"
)

// heartbeat to etcd cluster until stop
func Heartbeat(client *etcd.Client, name string, taskID uint64, interval time.Duration, stop chan struct{}) error {
	for {
		_, err := client.Set(HealthyPath(name, taskID), "health", computeTTL(interval))
		if err != nil {
			return err
		}
		select {
		case <-time.After(interval):
		case <-stop:
			return nil
		}
	}
}

// detect failure of the given taskID
func DetectFailure(client *etcd.Client, name string, taskID uint64, stop chan bool) (uint64, error) {
	key := HealthyPath(name, taskID)
	resp, err := client.Get(key, false, false)
	if err != nil {
		// TODO: should check "key not found"
		return taskID, nil
	}
	waitIndex := resp.EtcdIndex + 1
	for {
		resp, err = client.Watch(key, waitIndex, false, nil, stop)
		if err != nil {
			// on client closing
			return 0, err
		}
		if resp.Action == "delete" || resp.Action == "expire" {
			return taskID, nil
		}
		waitIndex = resp.EtcdIndex + 1
	}
}

// report failure to etcd cluster
// If a framework detects a failure, it tries to report failure to /failedTasks/{taskID}
func ReportFailure(client *etcd.Client, name string, taskID uint64) {

}

// WaitFailure blocks until it gets a hint of taks failure
func WaitFailure(client *etcd.Client, name string) uint64 {
	return 1
}

func computeTTL(interval time.Duration) uint64 {
	return uint64(math.Min(5*float64(interval/time.Second), 1))
}