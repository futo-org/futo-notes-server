package mockweb_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/mockweb"
)

const userID = "0195e2a1-0000-7000-8000-000000000001"

func TestLinksNameTheirCustomer(t *testing.T) {
	mock := mockweb.New("https://api.test", []byte("mock-webhook-secret"))
	for _, link := range []func() (string, error){
		func() (string, error) { return mock.CheckoutURL(t.Context(), billing.Customer{UserID: userID}) },
		func() (string, error) { return mock.PortalURL(t.Context(), billing.Customer{UserID: userID}) },
	} {
		raw, err := link()
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(raw)
		if err != nil || !strings.HasPrefix(raw, "https://api.test/dev/billing/") {
			t.Fatalf("link %s (%v)", raw, err)
		}
		if got, ok := mock.Customer(u.Query()); !ok || got != userID {
			t.Fatalf("Customer(%s) = %q, %t", raw, got, ok)
		}
		forged := u.Query()
		forged.Set("customer", "0195e2a1-0000-7000-8000-000000000002")
		if _, ok := mock.Customer(forged); ok {
			t.Fatal("a link re-pointed at another customer verified")
		}
	}
}

func TestCheckoutDeliveryVerifies(t *testing.T) {
	mock := mockweb.New("https://api.test", []byte("mock-webhook-secret"))
	sub := mock.Checkout(userID)
	delivery, err := mock.Delivery(sub)
	if err != nil {
		t.Fatal(err)
	}
	id, events, err := mock.Verify(t.Context(), delivery)
	if err != nil || id == "" || len(events) != 1 {
		t.Fatalf("Verify = %q, %v, %v", id, events, err)
	}
	ev := events[0]
	if ev.Source != billing.SourceWeb || ev.ExternalID != sub.ID || ev.UserID != userID || ev.Status != billing.StatusActive ||
		!ev.WillRenew || ev.ExpiresAt == nil || !ev.ExpiresAt.Equal(sub.ExpiresAt) || !ev.EventAt.Equal(sub.UpdatedAt) {
		t.Fatalf("event = %+v for %+v", ev, sub)
	}

	other := mockweb.New("https://api.test", []byte("another-webhook-secret"))
	if _, _, err := other.Verify(t.Context(), delivery); !errors.Is(err, billing.ErrInvalidDelivery) {
		t.Fatalf("Verify under another secret = %v, want ErrInvalidDelivery", err)
	}
	tampered := delivery
	tampered.Body = []byte(strings.Replace(string(delivery.Body), `"active"`, `"revoked"`, 1))
	if _, _, err := mock.Verify(t.Context(), tampered); !errors.Is(err, billing.ErrInvalidDelivery) {
		t.Fatalf("Verify of a tampered body = %v, want ErrInvalidDelivery", err)
	}
}

func TestCancelAndRefresh(t *testing.T) {
	mock := mockweb.New("https://api.test", []byte("mock-webhook-secret"))
	sub := mock.Checkout(userID)
	if err := mock.Cancel(t.Context(), sub.ID); err != nil {
		t.Fatal(err)
	}
	ev, err := mock.Refresh(t.Context(), sub.ID)
	if err != nil || ev.WillRenew || ev.Status != billing.StatusActive || ev.EventAt.Before(sub.UpdatedAt) {
		t.Fatalf("Refresh after Cancel = %+v, %v", ev, err)
	}
	if _, err := mock.Refresh(t.Context(), "mock_sub_unknown"); !errors.Is(err, mockweb.ErrUnknownSubscription) {
		t.Fatalf("Refresh of an unknown id = %v", err)
	}
	if err := mock.Cancel(t.Context(), "mock_sub_unknown"); !errors.Is(err, mockweb.ErrUnknownSubscription) {
		t.Fatalf("Cancel of an unknown id = %v", err)
	}
}
