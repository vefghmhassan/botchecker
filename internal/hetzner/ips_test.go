package hetzner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeCloud is enough of the Hetzner API to exercise reserving an address and
// creating a server that carries it.
type fakeCloud struct {
	mu sync.Mutex
	// pool is handed out in order, so a test can make the API return an address
	// it has already thrown away — which is what Hetzner actually does.
	pool      []string
	nextID    int64
	deleted   []int64
	created   []map[string]any
	dcName    string
	serverTyp string
}

func (f *fakeCloud) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/primary_ips", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.nextID++
		addr := "203.0.113.1"
		if len(f.pool) > 0 {
			addr, f.pool = f.pool[0], f.pool[1:]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"primary_ip": map[string]any{
				"id": f.nextID, "ip": addr,
				"datacenter": map[string]any{"name": f.dcName},
			},
		})
	})

	mux.HandleFunc("/primary_ips/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		var id int64
		_, _ = fmt.Sscan(r.URL.Path[len("/primary_ips/"):], &id)
		f.deleted = append(f.deleted, id)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/datacenters", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"datacenters": []map[string]any{{
				"id": 1, "name": f.dcName,
				"location":     map[string]any{"name": "hel1"},
				"server_types": map[string]any{"available": []int64{42}},
			}},
			"meta": map[string]any{"pagination": map[string]any{"next_page": nil}},
		})
	})

	mux.HandleFunc("/server_types", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"server_types": []map[string]any{{"id": 42, "name": f.serverTyp}},
			"meta":         map[string]any{"pagination": map[string]any{"next_page": nil}},
		})
	})

	mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got map[string]any
		_ = json.Unmarshal(body, &got)
		f.mu.Lock()
		f.created = append(f.created, got)
		f.nextID++
		id := f.nextID
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"server": map[string]any{"id": id, "name": "s",
				"public_net": map[string]any{"ipv4": map[string]any{"ip": "198.51.100.7"}}},
			"action": map[string]any{"id": 1, "status": "running"},
		})
	})

	return mux
}

func TestAReservedAddressIsVisibleBeforeAnythingIsBought(t *testing.T) {
	f := &fakeCloud{pool: []string{"5.161.158.200"}, dcName: "hel1-dc2", serverTyp: "cpx32"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv.URL, "tok")

	ip, err := c.CreatePrimaryIP(context.Background(), "n", "hel1-dc2", true)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// This is the whole point: the address is known while nothing but an IP
	// reservation exists, so refusing it costs one delete rather than a server
	// slot and twelve minutes of probing.
	if ip.Address != "5.161.158.200" {
		t.Fatalf("want the pooled address, got %q", ip.Address)
	}
	if ip.ID == 0 {
		t.Fatal("no id came back, so the reservation could never be released")
	}
	if len(f.created) != 0 {
		t.Fatalf("reserving an address must not create a server, got %d", len(f.created))
	}
}

func TestAServerIsCreatedInTheDatacenterThatHoldsItsAddress(t *testing.T) {
	f := &fakeCloud{dcName: "hel1-dc2", serverTyp: "cpx32"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv.URL, "tok")

	_, err := c.CreateFromSnapshot(context.Background(), CreateSpec{
		Name: "x", ServerType: "cpx32", Image: "1",
		// Location is deliberately set as well: a reserved address belongs to
		// one datacenter, and sending both is what Hetzner rejects.
		Location: "hel1", Datacenter: "hel1-dc2", PrimaryIPv4: 77,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got := f.created[0]
	if _, ok := got["location"]; ok {
		t.Error("location was sent alongside datacenter, which Hetzner refuses")
	}
	if got["datacenter"] != "hel1-dc2" {
		t.Errorf("datacenter not pinned: %v", got["datacenter"])
	}
	pub, ok := got["public_net"].(map[string]any)
	if !ok {
		t.Fatalf("the reserved address was not attached: %v", got)
	}
	if pub["ipv4"].(float64) != 77 || pub["enable_ipv4"] != true {
		t.Errorf("wrong public_net: %v", pub)
	}
}

func TestDatacenterForRefusesALocationThatCannotBuildTheType(t *testing.T) {
	f := &fakeCloud{dcName: "hel1-dc2", serverTyp: "cpx32"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv.URL, "tok")

	// Asking for a type the location does not offer used to be discovered only
	// after a failed create had already consumed an attempt.
	if _, err := c.DatacenterFor(context.Background(), "hel1", "cpx99"); err == nil {
		t.Fatal("a type Hetzner does not offer must be refused, not attempted")
	}
	if _, err := c.DatacenterFor(context.Background(), "ash", "cpx32"); err == nil {
		t.Fatal("a location with no datacenter for this type must be refused")
	}
	dc, err := c.DatacenterFor(context.Background(), "hel1", "cpx32")
	if err != nil || dc != "hel1-dc2" {
		t.Fatalf("want hel1-dc2, got %q (%v)", dc, err)
	}
}
