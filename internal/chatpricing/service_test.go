package chatpricing

import (
	"math"
	"testing"
)

func TestCalculateRoundsEachTokenClassUp(t *testing.T) {
	rates := Rates{InputCost: 1000, InputSale: 2000, CachedInputCost: 500, CachedInputSale: 1000, OutputCost: 3000, OutputSale: 6000}
	amounts, err := Calculate(rates, Usage{PromptTokens: 1001, CachedInputTokens: 1, CompletionTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if amounts.Cost != 3 || amounts.Sale != 4 {
		t.Fatalf("amounts=%+v", amounts)
	}
}
func TestCalculateRejectsInvalidAndOverflow(t *testing.T) {
	if _, err := Calculate(Rates{InputSale: 1, CachedInputSale: 1, OutputSale: 1}, Usage{PromptTokens: 1, CachedInputTokens: 2}); err == nil {
		t.Fatal("inconsistent cached usage accepted")
	}
	if _, ok := tokenAmount(math.MaxInt64, math.MaxInt64); ok {
		t.Fatal("overflow accepted")
	}
}
func TestMarginAppliesToEveryDimension(t *testing.T) {
	good := Rates{InputCost: 100, InputSale: 110, CachedInputCost: 50, CachedInputSale: 55, OutputCost: 200, OutputSale: 220}
	if !ratesMargin(good, 1000) {
		t.Fatal("valid margin rejected")
	}
	bad := good
	bad.CachedInputSale = 54
	if ratesMargin(bad, 1000) {
		t.Fatal("invalid cached margin accepted")
	}
}
