package memory

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestConcurrentRememberPersistsCurrentProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.md")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := p.Remember(fmt.Sprintf("fact %d", i)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	reloaded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reloaded.Facts(), p.Facts(); len(got) != 30 || !reflect.DeepEqual(got, want) {
		t.Fatalf("disk=%v memory=%v", got, want)
	}
}
