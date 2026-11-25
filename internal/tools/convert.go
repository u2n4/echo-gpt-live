package tools

// Fast kitchen, oven temperature, weight/volume, and currency conversions ($0).
// Kitchen & temperature run offline in 0 ms; currency rates are fetched from free keyless
// open.er-api.com/v6/latest/SAR and cached for 12 hours.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

var fxBase = "https://open.er-api.com/v6/latest/SAR"

type fxCache struct {
	mu      sync.Mutex
	fetched time.Time
	rates   map[string]float64 // 1 SAR -> X currency
}

// Convert builds the kitchen, temperature, weight, and currency conversion tool.
func Convert(client *http.Client) *Tool {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	cache := &fxCache{}
	return &Tool{
		Name: "convert",
		Description: "Instant kitchen/cooking conversions (cups, tablespoons, teaspoons, ml, liters, grams, kg, oz, lb, " +
			"and oven temperatures Fahrenheit <-> Celsius) plus live currency conversions to/from Saudi Riyals (SAR) " +
			"for USD, EUR, GBP, AED, KWD, BHD, QAR, OMR, EGP, TRY, IQD, JOD, etc. " +
			"Answers '350 فهرنهايت كم بالمئوي', 'كوب طحين كم غرام', 'ملعقتين كبيرة كم مل', '100 دولار كم ريال'.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"amount": map[string]any{
					"type":        "number",
					"description": "numeric value to convert",
				},
				"from": map[string]any{
					"type":        "string",
					"description": "source unit or ISO currency code: f, c, cup, tbsp, tsp, ml, l, g, kg, oz, lb, USD, SAR, EUR, GBP, AED, KWD, BHD, QAR, OMR, EGP, TRY, etc.",
				},
				"to": map[string]any{
					"type":        "string",
					"description": "target unit or ISO currency code (default SAR for currencies, c for f, g/ml for kitchen)",
				},
				"ingredient": map[string]any{
					"type":        "string",
					"description": "optional ingredient when converting kitchen volume to grams: water, flour, sugar, butter, oil, rice, milk",
				},
			},
			"required": []string{"amount", "from", "to"},
		},
		Access:       Read,
		DirectReturn: true,
		Timeout:      6 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			amount := floatArg(args["amount"])
			from := normUnit(str(args["from"]))
			to := normUnit(str(args["to"]))
			ing := strings.ToLower(strings.TrimSpace(str(args["ingredient"])))
			if from == "" || to == "" {
				return "", fmt.Errorf("حددي الوحدة أو العملة اللي تبين تحويلها")
			}

			// 1. Oven / temperature (offline)
			if (from == "f" || from == "c") && (to == "f" || to == "c") {
				if from == to {
					return fmt.Sprintf("%.0f درجة.", amount), nil
				}
				if from == "f" {
					c := (amount - 32) * 5 / 9
					return fmt.Sprintf("%.0f فهرنهايت تساوي حوالي %.0f درجة مئوية.", amount, math.Round(c)), nil
				}
				f := amount*9/5 + 32
				return fmt.Sprintf("%.0f درجة مئوية تساوي حوالي %.0f فهرنهايت.", amount, math.Round(f)), nil
			}

			// 2. Weight <-> Weight (offline)
			if wf, ok1 := weightGrams[from]; ok1 {
				if wt, ok2 := weightGrams[to]; ok2 {
					res := amount * wf / wt
					return fmt.Sprintf("%s %s تساوي %s %s.", trimFloat(amount), unitArabic(from), trimFloat(res), unitArabic(to)), nil
				}
			}

			// 3. Volume <-> Volume (offline)
			if vf, ok1 := volumeML[from]; ok1 {
				if vt, ok2 := volumeML[to]; ok2 {
					res := amount * vf / vt
					return fmt.Sprintf("%s %s تساوي %s %s.", trimFloat(amount), unitArabic(from), trimFloat(res), unitArabic(to)), nil
				}
				// Volume -> Weight using ingredient density (default water/milk ~ 1g/ml)
				if wt, ok2 := weightGrams[to]; ok2 {
					density, ingAr := ingredientDensity(ing)
					grams := amount * vf * density
					res := grams / wt
					return fmt.Sprintf("%s %s %s تساوي حوالي %s %s.", trimFloat(amount), unitArabic(from), ingAr, trimFloat(res), unitArabic(to)), nil
				}
			}
			// Weight -> Volume
			if wf, ok1 := weightGrams[from]; ok1 {
				if vt, ok2 := volumeML[to]; ok2 {
					density, ingAr := ingredientDensity(ing)
					ml := (amount * wf) / density
					res := ml / vt
					return fmt.Sprintf("%s %s %s تساوي حوالي %s %s.", trimFloat(amount), unitArabic(from), ingAr, trimFloat(res), unitArabic(to)), nil
				}
			}

			// 4. Currency (live free API with 12h cache)
			cf, ct := strings.ToUpper(from), strings.ToUpper(to)
			if len(cf) == 3 && len(ct) == 3 {
				return convertCurrency(ctx, client, cache, amount, cf, ct)
			}
			return "", fmt.Errorf("ما أقدر أحوّل من %s إلى %s", from, to)
		},
	}
}

func convertCurrency(ctx context.Context, client *http.Client, cache *fxCache, amount float64, from, to string) (string, error) {
	if from == to {
		return fmt.Sprintf("%s %s.", trimFloat(amount), currencyArabic(to)), nil
	}
	cache.mu.Lock()
	rates := cache.rates
	stale := len(rates) == 0 || time.Since(cache.fetched) > 12*time.Hour
	cache.mu.Unlock()

	if stale {
		body, err := get(ctx, client, fxBase, 128<<10, nil)
		if err != nil {
			return "", fmt.Errorf("ما قدرت أجيب أسعار الصرف الحين: %w", err)
		}
		var r struct {
			Result string             `json:"result"`
			Rates  map[string]float64 `json:"rates"`
		}
		if json.Unmarshal(body, &r) != nil || len(r.Rates) == 0 {
			return "", fmt.Errorf("رد أسعار الصرف غير مقروء")
		}
		cache.mu.Lock()
		cache.rates = r.Rates
		cache.fetched = time.Now()
		rates = r.Rates
		cache.mu.Unlock()
	}

	rf, ok1 := rates[from]
	rt, ok2 := rates[to]
	if !ok1 || !ok2 || rf <= 0 {
		return "", fmt.Errorf("العملة %s أو %s غير متوفرة", from, to)
	}
	// Rates are per 1 SAR: amount_in_SAR = amount / rf, then * rt.
	res := (amount / rf) * rt
	return fmt.Sprintf("%s %s تساوي تقريباً %s %s.", trimFloat(amount), currencyArabic(from), trimFloat(res), currencyArabic(to)), nil
}

var weightGrams = map[string]float64{
	"g": 1, "kg": 1000, "oz": 28.3495, "lb": 453.592,
}

var volumeML = map[string]float64{
	"ml": 1, "l": 1000, "tsp": 5, "tbsp": 15, "cup": 240,
}

func ingredientDensity(ing string) (float64, string) {
	switch normToolArabic(ing) {
	case "flour", "طحين", "دقيق":
		return 125.0 / 240.0, "طحين"
	case "sugar", "سكر":
		return 200.0 / 240.0, "سكر"
	case "butter", "زبده":
		return 227.0 / 240.0, "زبدة"
	case "oil", "زيت":
		return 218.0 / 240.0, "زيت"
	case "rice", "رز", "ارز":
		return 190.0 / 240.0, "رز"
	case "honey", "عسل":
		return 340.0 / 240.0, "عسل"
	case "milk", "حليب":
		return 1.03, "حليب"
	default:
		return 1.0, "(بمقياس الماء)"
	}
}

func normUnit(u string) string {
	s := normToolArabic(u)
	switch s {
	case "f", "fahrenheit", "فهرنهايت":
		return "f"
	case "c", "celsius", "مئوي", "مئويه", "ميوي", "ميويه", "سيليزيوس":
		return "c"
	case "cup", "cups", "كوب", "اكواب", "كاس", "كاسه":
		return "cup"
	case "tbsp", "tablespoon", "tablespoons", "ملعقه كبيره", "ملاعق كبيره", "ملعقه طعام":
		return "tbsp"
	case "tsp", "teaspoon", "teaspoons", "ملعقه صغيره", "ملاعق صغيره", "ملعقه شاي":
		return "tsp"
	case "ml", "مل", "ملي", "مليلتر":
		return "ml"
	case "l", "liter", "litre", "لتر":
		return "l"
	case "g", "gram", "grams", "غرام", "جرام":
		return "g"
	case "kg", "kilo", "kilogram", "كيلو", "كيلوغرام":
		return "kg"
	case "oz", "ounce", "ounces", "اونصه", "اوقيه":
		return "oz"
	case "lb", "lbs", "pound", "pounds", "رطل", "باوند":
		return "lb"
	case "ريال", "ريال سعودي", "sar":
		return "SAR"
	case "دولار", "usd":
		return "USD"
	case "يورو", "eur":
		return "EUR"
	case "جنيه استرليني", "باوند استرليني", "gbp":
		return "GBP"
	case "درهم", "درهم اماراتي", "aed":
		return "AED"
	case "دينار كويتي", "kwd":
		return "KWD"
	case "دينار بحريني", "bhd":
		return "BHD"
	case "ريال قطري", "qar":
		return "QAR"
	case "ريال عماني", "omr":
		return "OMR"
	case "جنيه مصري", "egp":
		return "EGP"
	case "ليره تركيه", "try":
		return "TRY"
	case "دينار عراقي", "iqd":
		return "IQD"
	case "دينار اردني", "jod":
		return "JOD"
	default:
		return strings.TrimSpace(u)
	}
}

func unitArabic(u string) string {
	switch u {
	case "cup":
		return "كوب"
	case "tbsp":
		return "ملعقة كبيرة"
	case "tsp":
		return "ملعقة صغيرة"
	case "ml":
		return "مل"
	case "l":
		return "لتر"
	case "g":
		return "غرام"
	case "kg":
		return "كيلو"
	case "oz":
		return "أونصة"
	case "lb":
		return "رطل"
	default:
		return u
	}
}

func currencyArabic(c string) string {
	switch c {
	case "SAR":
		return "ريال سعودي"
	case "USD":
		return "دولار"
	case "EUR":
		return "يورو"
	case "GBP":
		return "جنيه إسترليني"
	case "AED":
		return "درهم إماراتي"
	case "KWD":
		return "دينار كويتي"
	case "BHD":
		return "دينار بحريني"
	case "QAR":
		return "ريال قطري"
	case "OMR":
		return "ريال عماني"
	case "EGP":
		return "جنيه مصري"
	case "TRY":
		return "ليرة تركية"
	case "IQD":
		return "دينار عراقي"
	case "JOD":
		return "دينار أردني"
	default:
		return c
	}
}

func trimFloat(v float64) string {
	if math.Abs(v-math.Round(v)) < 0.05 {
		return fmt.Sprintf("%.0f", math.Round(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}
