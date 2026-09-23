package hetzner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// This file covers the address resources: reserving a primary IP before a
// server exists, and creating and moving a floating IP.
//
// Both exist for the same reason. Hetzner returns the IP of a deleted server to
// its location's pool, so a replacement built where the blocked one stood can
// be handed the blocked address straight back. Seeing the address before
// committing to it is the only way to refuse it cheaply.

// CreatePrimaryIP reserves an IPv4 in a datacenter without assigning it.
//
// autoDelete decides what happens when the server carrying it is destroyed:
// true lets it go with the server, which is what an ordinary replacement wants.
// Hetzner bills every primary IP that finished creating, assigned or not, so an
// address that is rejected must be deleted rather than abandoned.
func (c *Client) CreatePrimaryIP(ctx context.Context, name, datacenter string, autoDelete bool) (*PrimaryIP, error) {
	body := map[string]any{
		"type":          "ipv4",
		"assignee_type": "server",
		"name":          name,
		"datacenter":    datacenter,
		"auto_delete":   autoDelete,
	}
	var resp struct {
		PrimaryIP apiIPResource `json:"primary_ip"`
	}
	if err := c.do(ctx, http.MethodPost, "/primary_ips", body, &resp); err != nil {
		return nil, err
	}
	return &PrimaryIP{
		ID:         resp.PrimaryIP.ID,
		Address:    normalise(resp.PrimaryIP.IP),
		Datacenter: resp.PrimaryIP.Datacenter.Name,
		AssigneeID: resp.PrimaryIP.AssigneeID,
	}, nil
}

// DeletePrimaryIP releases a reserved address. Only an unassigned one can go.
func (c *Client) DeletePrimaryIP(ctx context.Context, id int64) error {
	c.InvalidateInventory()
	return c.do(ctx, http.MethodDelete, "/primary_ips/"+strconv.FormatInt(id, 10), nil, nil)
}

// CreateFloatingIP reserves a movable address, homed in a location but attached
// to nothing. Creating it unassigned is deliberate: the address is readable
// before it is pointed at a machine, so a recycled one can be thrown back
// without anything having been touched.
func (c *Client) CreateFloatingIP(ctx context.Context, description, homeLocation string) (*FloatingIP, error) {
	body := map[string]any{
		"type":          "ipv4",
		"description":   description,
		"home_location": homeLocation,
	}
	var resp struct {
		FloatingIP apiIPResource `json:"floating_ip"`
	}
	if err := c.do(ctx, http.MethodPost, "/floating_ips", body, &resp); err != nil {
		return nil, err
	}
	return &FloatingIP{
		ID:           resp.FloatingIP.ID,
		Address:      normalise(resp.FloatingIP.IP),
		HomeLocation: resp.FloatingIP.HomeLocation.Name,
		ServerID:     resp.FloatingIP.Server,
	}, nil
}

// AssignFloatingIP points a floating IP at a server. The server keeps running:
// unlike a primary IP this needs no power cycle — but the address does not
// answer until it is configured inside the server's operating system.
func (c *Client) AssignFloatingIP(ctx context.Context, id, serverID int64) (int64, error) {
	var resp struct {
		Action struct {
			ID int64 `json:"id"`
		} `json:"action"`
	}
	path := "/floating_ips/" + strconv.FormatInt(id, 10) + "/actions/assign"
	if err := c.do(ctx, http.MethodPost, path, map[string]any{"server": serverID}, &resp); err != nil {
		return 0, err
	}
	c.InvalidateInventory()
	return resp.Action.ID, nil
}

// UnassignFloatingIP detaches a floating IP from whatever holds it.
func (c *Client) UnassignFloatingIP(ctx context.Context, id int64) error {
	path := "/floating_ips/" + strconv.FormatInt(id, 10) + "/actions/unassign"
	if err := c.do(ctx, http.MethodPost, path, map[string]any{}, nil); err != nil {
		return err
	}
	c.InvalidateInventory()
	return nil
}

// DeleteFloatingIP gives a floating address back. Like primary IPs these are
// billed while they exist, so a rejected one is deleted rather than kept.
func (c *Client) DeleteFloatingIP(ctx context.Context, id int64) error {
	c.InvalidateInventory()
	return c.do(ctx, http.MethodDelete, "/floating_ips/"+strconv.FormatInt(id, 10), nil, nil)
}

// Datacenters lists the sites, with the server types each can create right now.
func (c *Client) Datacenters(ctx context.Context) ([]Datacenter, error) {
	var out []Datacenter
	err := c.paginate(ctx, "/datacenters", "datacenters", func(raw json.RawMessage) error {
		var page []struct {
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			Location struct {
				Name string `json:"name"`
			} `json:"location"`
			ServerTypes struct {
				Available []int64 `json:"available"`
			} `json:"server_types"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, d := range page {
			dc := Datacenter{
				ID: d.ID, Name: d.Name, Location: d.Location.Name,
				AvailableTypes: make(map[int64]bool, len(d.ServerTypes.Available)),
			}
			for _, id := range d.ServerTypes.Available {
				dc.AvailableTypes[id] = true
			}
			out = append(out, dc)
		}
		return nil
	})
	return out, err
}

// ServerTypes maps a server type name to its id, which is what the datacenter
// availability lists are keyed by.
func (c *Client) ServerTypes(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	err := c.paginate(ctx, "/server_types", "server_types", func(raw json.RawMessage) error {
		var page []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, t := range page {
			out[t.Name] = t.ID
		}
		return nil
	})
	return out, err
}

// DatacenterFor picks a site in the given location that can create the given
// server type. An empty location means the caller has no preference, so the
// first datacenter offering the type wins.
func (c *Client) DatacenterFor(ctx context.Context, location, serverType string) (string, error) {
	dcs, err := c.Datacenters(ctx)
	if err != nil {
		return "", err
	}
	types, err := c.ServerTypes(ctx)
	if err != nil {
		return "", err
	}
	typeID, ok := types[serverType]
	if !ok {
		return "", fmt.Errorf("hetzner does not offer a server type called %q", serverType)
	}

	for _, dc := range dcs {
		if location != "" && dc.Location != location {
			continue
		}
		if dc.AvailableTypes[typeID] {
			return dc.Name, nil
		}
	}
	return "", fmt.Errorf("no datacenter in %q can create a %s right now", location, serverType)
}

// apiIPResource is the shape primary and floating IPs share.
type apiIPResource struct {
	ID         int64  `json:"id"`
	IP         string `json:"ip"`
	AssigneeID int64  `json:"assignee_id"`
	Server     int64  `json:"server"`
	Datacenter struct {
		Name string `json:"name"`
	} `json:"datacenter"`
	HomeLocation struct {
		Name string `json:"name"`
	} `json:"home_location"`
}
