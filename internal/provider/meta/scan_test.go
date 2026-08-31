package meta

import (
	"reflect"
	"testing"
)

func TestFindObjectHandlesNestedAndStrings(t *testing.T) {
	body := []byte(`{"junk":{"a":1},"shortcode_media":{"id":"1","caption":"}{ tricky \" brace","owner":{"id":"2"}},"tail":9}`)
	obj := findObject(body, "shortcode_media")
	if obj == nil {
		t.Fatal("findObject returned nil")
	}
	want := `{"id":"1","caption":"}{ tricky \" brace","owner":{"id":"2"}}`
	if string(obj) != want {
		t.Errorf("findObject =\n%s\nwant\n%s", obj, want)
	}
}

func TestFindObjectSkipsNonObjectValues(t *testing.T) {
	// The first occurrence of the key holds a string, the second an object.
	body := []byte(`{"feedback":"none","other":1,"feedback":{"reaction_count":{"count":7}}}`)
	obj := findObject(body, "feedback")
	if obj == nil {
		t.Fatal("findObject returned nil for the object-valued occurrence")
	}
	if got := countInObject(obj, "reaction_count"); got == nil || *got != 7 {
		t.Errorf("countInObject = %v, want 7", got)
	}
}

func TestFindObjectMissingKey(t *testing.T) {
	if obj := findObject([]byte(`{"a":1}`), "nope"); obj != nil {
		t.Errorf("findObject on a missing key = %s, want nil", obj)
	}
}

func TestBalancedObjectTruncated(t *testing.T) {
	if obj := balancedObject([]byte(`{"a":{"b":1}`)); obj != nil {
		t.Errorf("balancedObject on truncated input = %s, want nil", obj)
	}
}

func TestUnescapeJSONString(t *testing.T) {
	in := []byte(`<script>x={"contextJSON":"{\"shortcode_media\":{\"id\":\"7\"}}"}</script>`)
	out := unescapeJSONString(in)
	obj := findObject(out, "shortcode_media")
	if obj == nil {
		t.Fatalf("shortcode_media not findable after unescaping: %s", out)
	}
	var media shortcodeMedia
	if !decodeInto(obj, &media) || media.ID != "7" {
		t.Errorf("decoded media = %+v, want ID 7", media)
	}
}

func TestNumberAfterKey(t *testing.T) {
	body := []byte(`{"video_view_count":123456,"play_count":"789"}`)
	if got := numberAfterKey(body, "video_view_count"); got == nil || *got != 123456 {
		t.Errorf("numeric form = %v, want 123456", got)
	}
	if got := numberAfterKey(body, "play_count"); got == nil || *got != 789 {
		t.Errorf("string form = %v, want 789", got)
	}
	if got := numberAfterKey(body, "absent"); got != nil {
		t.Errorf("absent key = %v, want nil", got)
	}
}

func TestMetaContent(t *testing.T) {
	body := []byte(`
	  <meta property="og:title" content="A &amp; B">
	  <meta content="1,234 likes" name="og:description"/>
	`)
	if got := metaContent(body, "og:title"); got != "A & B" {
		t.Errorf("og:title = %q, want %q", got, "A & B")
	}
	if got := metaContent(body, "og:description"); got != "1,234 likes" {
		t.Errorf("reversed attribute order = %q, want %q", got, "1,234 likes")
	}
	if got := metaContent(body, "og:image"); got != "" {
		t.Errorf("absent tag = %q, want empty", got)
	}
}

func TestParseHumanCounts(t *testing.T) {
	tests := []struct {
		desc string
		want map[string]uint64
	}{
		{"1,234 likes, 56 comments - natgeo on January 1, 2024", map[string]uint64{"likes": 1234, "comments": 56}},
		{"1.2M views, 40K likes", map[string]uint64{"views": 1200000, "likes": 40000}},
		{"2.5B plays", map[string]uint64{"views": 2500000000}},
		{"no counters at all", map[string]uint64{}},
	}
	for _, tc := range tests {
		if got := parseHumanCounts(tc.desc); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseHumanCounts(%q) = %v, want %v", tc.desc, got, tc.want)
		}
	}
}

func TestParseHumanNumberKeepsUnabbreviatedValuesExact(t *testing.T) {
	// A float round-trip would lose the last digits of a large exact figure.
	got, ok := parseHumanNumber("18,446,744,073", "")
	if !ok || got != 18446744073 {
		t.Errorf("parseHumanNumber = (%d, %v), want 18446744073", got, ok)
	}
}
