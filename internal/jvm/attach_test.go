package jvm

import "testing"

func TestParseHistogram(t *testing.T) {
	out := ` num     #instances         #bytes  class name (module)
-------------------------------------------------------
   1:          5120      536952832  [B (java.base@21.0.4)
   2:         13021         416672  java.lang.String (java.base@21.0.4)
   3:            12          49440  [Ljava.lang.Object; (java.base@21.0.4)
Total         18153      537418944
`
	e, total := ParseHistogram(out)
	if len(e) != 3 || total != 537418944 {
		t.Fatalf("got %d entries total %d", len(e), total)
	}
	if e[0].Class != "byte[]" || e[0].Bytes != 536952832 || e[0].Instances != 5120 {
		t.Fatalf("row 1 = %+v", e[0])
	}
	if e[2].Class != "java.lang.Object[]" {
		t.Fatalf("row 3 class = %q", e[2].Class)
	}
}

func TestAppClassesSkipsJDK(t *testing.T) {
	e := []HistogramEntry{{Class: "byte[]"}, {Class: "java.lang.String"}, {Class: "com.example.Cart"}, {Class: "jdk.internal.Foo"}, {Class: "org.acme.Session"}}
	got := AppClasses(e, 3)
	if len(got) != 2 || got[0].Class != "com.example.Cart" || got[1].Class != "org.acme.Session" {
		t.Fatalf("got %+v", got)
	}
}
