package dialect

import "testing"

func TestKnownCoReviewersHaveVendorDescriptors(t *testing.T) {
	for _, reviewer := range KnownCoReviewers() {
		t.Run(reviewer.Name, func(t *testing.T) {
			vendor, ok := VendorFor(reviewer.Name)
			if !ok || vendor.Site == "" || vendor.Docs == "" {
				t.Fatalf("VendorFor(%q) = %#v, %v; want a non-empty descriptor", reviewer.Name, vendor, ok)
			}
		})
	}
}
