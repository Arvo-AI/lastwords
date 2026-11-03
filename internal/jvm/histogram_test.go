package jvm

import "testing"

// Excerpts of real GC.class_histogram -all output from the EKS run.
const g1Indexer = ` num     #instances         #bytes  class name (module)
-------------------------------------------------------
   1:         58932      925179288  [J (java.base@21.0.13)
   2:          1771       30314448  [Ljdk.internal.vm.FillerElement; (java.base@21.0.13)
   3:         72681        2469320  [B (java.base@21.0.13)
   4:         72525        1740600  java.lang.String (java.base@21.0.13)
   5:         56401        1353624  Indexer$Posting
   6:          2533         705712  [Ljava.lang.Object; (java.base@21.0.13)
Total        264843      961762992
`

const cartSnapshots = ` num     #instances         #bytes  class name (module)
-------------------------------------------------------
   1:         13690      191191136  [B (java.base@21.0.13)
   2:           138        2672680  [Ljdk.internal.vm.FillerElement; (java.base@21.0.13)
   3:         10658         255792  java.lang.String (java.base@21.0.13)
   4:          3876         124032  java.util.concurrent.ConcurrentHashMap$Node (java.base@21.0.13)
   5:          2910          93120  com.example.checkout.CheckoutService$CartSnapshot
Total         31272      194336760
`

func TestFillerSpaceIsNotRankedAsObjects(t *testing.T) {
	entries, _ := ParseHistogram(g1Indexer)
	kept, dropped := DropFillers(entries)
	if dropped != 30314448 {
		t.Fatalf("dropped %d filler bytes, want 30314448", dropped)
	}
	for _, e := range kept {
		if e.Class == "jdk.internal.vm.FillerElement[]" {
			t.Fatal("filler still in the histogram")
		}
	}
}

func TestHolderNamesTheClassBehindALargeArray(t *testing.T) {
	entries, _ := ParseHistogram(g1Indexer)
	entries, _ = DropFillers(entries)
	h, ok := Holder(entries, entries[0])
	if !ok || h.Class != "Indexer$Posting" {
		t.Fatalf("holder of long[] = %q, %v; want Indexer$Posting", h.Class, ok)
	}
	// byte[] here is shared by Strings and snapshots: no one-to-one owner,
	// so no claim should be made.
	entries, _ = ParseHistogram(cartSnapshots)
	if h, ok := Holder(entries, entries[0]); ok {
		t.Fatalf("claimed %s holds byte[] without a matching count", h.Class)
	}
}
