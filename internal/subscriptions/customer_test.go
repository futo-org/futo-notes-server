package subscriptions_test

import (
	"context"
	"testing"

	"futo-notes-server/internal/subscriptions"
)

func TestAnAccountWithNoCheckoutYetHasNoPolarCustomer(t *testing.T) {
	database := newDatabase(t)
	userID := newAccount(t, database)

	customer, err := subscriptions.CustomerOf(context.Background(), database, userID)
	if err != nil {
		t.Fatalf("CustomerOf() error = %v", err)
	}
	if customer.ID != "" {
		t.Errorf("ID = %q before any checkout, want empty so the route creates one", customer.ID)
	}
	if customer.ExternalID != accountSub {
		t.Errorf("ExternalID = %q, want the account's sub", customer.ExternalID)
	}
	if customer.Email != "person@futo.org" {
		t.Errorf("Email = %q, want the account's email", customer.Email)
	}
}

func TestSavingTheCustomerIDLeavesTheAccountUnsubscribed(t *testing.T) {
	database := newDatabase(t)
	userID := newAccount(t, database)

	if err := subscriptions.SaveCustomerID(context.Background(), database, userID, "cus_1", noon); err != nil {
		t.Fatalf("SaveCustomerID() error = %v", err)
	}

	customer, err := subscriptions.CustomerOf(context.Background(), database, userID)
	if err != nil {
		t.Fatalf("CustomerOf() error = %v", err)
	}
	if customer.ID != "cus_1" {
		t.Errorf("ID = %q after saving one, want cus_1", customer.ID)
	}

	// A Polar customer is not a subscription. Nothing about the row it
	// created may entitle the account or look like a delivery arrived.
	record := recordOf(t, database, userID)
	if record.State != subscriptions.StateNone || record.Entitled(noon) {
		t.Errorf("record = %+v after only a customer, want an unentitled %q",
			record, subscriptions.StateNone)
	}
	if record.LastEvent != "" {
		t.Errorf("LastEvent = %q with no delivery received, want empty", record.LastEvent)
	}
}

func TestASubscriptionArrivingAfterTheCustomerKeepsBoth(t *testing.T) {
	database := newDatabase(t)
	userID := newAccount(t, database)

	if err := subscriptions.SaveCustomerID(context.Background(), database, userID, "cus_1", noon); err != nil {
		t.Fatalf("SaveCustomerID() error = %v", err)
	}
	if !apply(t, database, "subscription.active", "active", accountSub, noon) {
		t.Fatal("the delivery matched no account")
	}

	customer, err := subscriptions.CustomerOf(context.Background(), database, userID)
	if err != nil {
		t.Fatalf("CustomerOf() error = %v", err)
	}
	if customer.ID != "cus_1" {
		t.Errorf("ID = %q after a delivery, want the customer to survive it", customer.ID)
	}
	if record := recordOf(t, database, userID); !record.Entitled(noon) {
		t.Errorf("record = %+v, want the delivery to have entitled the account", record)
	}
}

func TestSavingTheCustomerIDDoesNotDisturbALiveSubscription(t *testing.T) {
	database := newDatabase(t)
	userID := newAccount(t, database)
	if !apply(t, database, "subscription.active", "active", accountSub, noon) {
		t.Fatal("the delivery matched no account")
	}

	// The portal route reaching an account Polar already told us about must
	// not roll its state back to unsubscribed.
	if err := subscriptions.SaveCustomerID(context.Background(), database, userID, "cus_1", noon); err != nil {
		t.Fatalf("SaveCustomerID() error = %v", err)
	}

	record := recordOf(t, database, userID)
	if record.State != "active" || !record.Entitled(noon) {
		t.Errorf("record = %+v after saving a customer id, want the subscription untouched", record)
	}
	if record.LastEvent == "" {
		t.Error("LastEvent was cleared by saving a customer id")
	}
}
