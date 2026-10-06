package catalog

import (
	"strings"
	"testing"
	"time"
)

const (
	cheapID  = "0xc2c4b037ff12e0aa81178deac52aeed902b36189b9e6feae22b72324c9221130"
	priceyID = "0xe79d9b0e5022c58535cf4373bd4b74ed4ca04dea76772b845bbe87a18c529e9c"
)

func bid(status, pps string) BidDetail {
	return BidDetail{Status: status, PricePerSecond: pps}
}

func twin(id, name string, bids ...BidDetail) Model {
	return Model{ID: id, Name: name, BidDetail: bids}
}

func loaded(models ...Model) *Catalog {
	c := New("http://catalog.invalid/models.json")
	c.models = append([]Model(nil), models...)
	c.byName, c.byID = indexModels(c.models)
	c.fetchedAt = time.Now()
	return c
}

func TestResolveUniqueNameAndRawID(t *testing.T) {
	c := loaded(twin(cheapID, "llama-3.3-70b", bid("healthy", "100")))
	id, err := c.Resolve("Llama-3.3-70b")
	if err != nil || id != cheapID {
		t.Fatalf("Resolve name: %q %v", id, err)
	}
	raw, err := c.Resolve("0xexplicit")
	if err != nil || raw != "0xexplicit" {
		t.Fatalf("Resolve raw id: %q %v", raw, err)
	}
	if _, err := c.Resolve("missing"); err == nil {
		t.Fatal("missing name should error")
	}
}

func TestResolveDuplicateNamePicksCheapestHealthy(t *testing.T) {
	cheap := twin(cheapID, "deepseek-v4-flash", bid("healthy", "104"))
	pricey := twin(priceyID, "deepseek-v4-flash", bid("Healthy", "950"))
	for _, order := range [][]Model{{pricey, cheap}, {cheap, pricey}} {
		c := loaded(order...)
		id, err := c.Resolve("deepseek-v4-flash")
		if err != nil {
			t.Fatal(err)
		}
		if id != cheapID {
			t.Fatalf("order %s then %s: got %s, want cheap twin", order[0].ID[:6], order[1].ID[:6], id)
		}
	}
}

func TestResolveHealthyBeatsCheaperUnhealthy(t *testing.T) {
	unhealthyCheap := twin(cheapID, "glm-5.2", bid("failed", "10"))
	healthyPricey := twin(priceyID, "glm-5.2", bid("healthy", "900"))
	c := loaded(unhealthyCheap, healthyPricey)
	id, err := c.Resolve("glm-5.2")
	if err != nil || id != priceyID {
		t.Fatalf("got %s %v, want healthy twin", id, err)
	}
}

func TestResolveUnhealthyFallsBackToCheapest(t *testing.T) {
	c := loaded(
		twin(priceyID, "gpt-5.4", bid("failed", "200")),
		twin(cheapID, "gpt-5.4", bid("failed", "50")),
	)
	id, err := c.Resolve("gpt-5.4")
	if err != nil || id != cheapID {
		t.Fatalf("got %s %v, want cheaper unhealthy twin", id, err)
	}
}

func TestResolveEqualPriceTieBreaksByID(t *testing.T) {
	// cheapID sorts before priceyID.
	c := loaded(
		twin(priceyID, "same", bid("healthy", "10")),
		twin(cheapID, "same", bid("healthy", "10")),
	)
	id, err := c.Resolve("same")
	if err != nil || id != cheapID {
		t.Fatalf("got %s %v, want lexicographically lower id", id, err)
	}
	if !strings.EqualFold(cheapID, c.byName["same"]) {
		t.Fatalf("byName = %s", c.byName["same"])
	}
}

func TestSendableID(t *testing.T) {
	m := twin(cheapID, "deepseek-v4-flash")
	if got := m.SendableID(false); got != "deepseek-v4-flash" {
		t.Fatalf("unique name: %s", got)
	}
	if got := m.SendableID(true); got != cheapID {
		t.Fatalf("shared name: %s", got)
	}
}
