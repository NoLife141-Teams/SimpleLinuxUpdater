package servers

import (
	"sync"
	"testing"
)

func TestStateFindByNameSupportsConcurrentInventoryChanges(t *testing.T) {
	var stateMu sync.Mutex
	serverList := []Server{{Name: "alpha"}}
	statusMap := map[string]*ServerStatus{}
	state := NewState(&stateMu, &serverList, &statusMap, nil)

	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := 0; i < 1000; i++ {
			state.Lock()
			if i%2 == 0 {
				state.SetServers([]Server{{Name: "alpha"}})
			} else {
				state.SetServers([]Server{{Name: "beta"}})
			}
			state.Unlock()
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 1000; i++ {
			_, _ = state.FindByName("alpha")
		}
	}()
	workers.Wait()
}
