package scenario

import "testing"

// — a chain http step's url may start with ${INGESTION_URL}; the validator accepts it
// (it is not an unresolved variable) and still refuses what it refused before.

func TestValidate_HTTPStepIngestionURLAccepted(t *testing.T) {
	trig := `{"steps":[
		{"type":"http","name":"login","method":"POST","url":"${INGESTION_URL}/api/v1/login","body":{"password":"${SOME_PASSWORD}"},"save":{"id":"id"}},
		{"type":"http","name":"read","method":"GET","url":"${INGESTION_URL}/api/v1/items/${saved.id}"}
	]}`
	_, errs := Validate(httpChainMD(trig, "### Runnable\n- step login: status=200\n- step read: status=200\n"))
	if len(errs) != 0 {
		t.Fatalf("a ${INGESTION_URL}-led http url, an env ${NAME} in the body and a ${saved.id} must be accepted, got %v", errs)
	}
}

// `target` stays refused on an http step, beside ${INGESTION_URL} or not: ${INGESTION_URL} is the plain
// targets.http base and a chain cannot select a named http target.
func TestValidate_HTTPStepTargetStillRefusedBesideIngestionURL(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"ping","method":"GET","target":"other","url":"${INGESTION_URL}/health"}]}`
	_, errs := Validate(httpChainMD(trig, "### Runnable\n- step ping: status=200\n"))
	if !find(errs, `carries the field "target"`) {
		t.Fatalf("`target` on an http step must be refused by name; got %v", errs)
	}
}

// The money guard reads the PATH after a leading ${INGESTION_URL}: the marker does not hide a money verb.
func TestMoneyGuard_IngestionURLDoesNotHideAMoneyPath(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"place","method":"GET","url":"${INGESTION_URL}/api/v1/orders"}]}`
	s, _ := Validate(httpChainMD(trig, "### Runnable\n- step place: status=200\n"))
	if s == nil {
		t.Fatal("scenario did not parse")
	}
	if v := MoneyGuardViolations(s, MoneyWriteAllowlist{}); len(v) == 0 {
		t.Fatalf("an order path behind ${INGESTION_URL} must still be refused on a money-handling system")
	}
}
