package longhorn

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestChangedPaths(t *testing.T) {
	var a, b map[string]any
	_ = json.Unmarshal([]byte(`{"metadata":{"resourceVersion":"1","managedFields":[{"manager":"m","time":"t1"}]},
		"status":{"actualSize":1,"robustness":"healthy","conditions":[{"type":"A","status":"True"}]}}`), &a)
	_ = json.Unmarshal([]byte(`{"metadata":{"resourceVersion":"2","managedFields":[{"manager":"m","time":"t2"}]},
		"status":{"actualSize":2,"robustness":"healthy","conditions":[{"type":"A","status":"True"}],"new":"x"}}`), &b)
	got := ChangedPaths(a, b)
	want := []string{"metadata.managedFields[*].time", "status.actualSize", "status.new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	var fs FieldStats
	fs.Add(got)
	fs.Add([]string{"status.actualSize"})
	if fs.Count["status.actualSize"] != 2 || fs.Updates != 2 {
		t.Errorf("%+v", fs)
	}
}
