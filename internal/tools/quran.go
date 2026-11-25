package tools

// Verified Holy Quran verses, concise Tafsir (via free keyless AlQuran Cloud API), and offline
// built-in daily Duas & Adhkar (including Tasbih al-Zahra, Dua al-Sabah, Dua al-Faraj, healing,
// travel, and bedtime adhkar) so nothing sacred is ever hallucinated.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var quranBase = "https://api.alquran.cloud/v1"

// QuranLookup returns the quran_lookup tool.
func QuranLookup(client *http.Client) *Tool {
	if client == nil {
		client = &http.Client{Timeout: 6 * time.Second}
	}
	return &Tool{
		Name: "quran_lookup",
		Description: "Verified Holy Quran verses with concise Arabic Tafsir (al-Muyassar) AND offline built-in " +
			"daily Duas and Adhkar (morning, evening, tasbih al-zahra, sleep, travel, healing, faraj). " +
			"Always use this when asked for a Quran verse, its meaning/tafsir, or a dua/dhikr so the text is exact. " +
			"Answers 'وش تفسير آية الكرسي', 'اقرئي لي سورة الإخلاص', 'عطيني أذكار الصباح', 'تسبيح الزهراء', 'دعاء الفرج', 'دعاء السفر', 'دعاء الشفاء'.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"mode": map[string]any{
					"type":        "string",
					"description": "ayah (lookup verse + tafsir), search (find verse by Arabic words), or adhkar (built-in duas/adhkar)",
					"enum":        []string{"ayah", "search", "adhkar"},
				},
				"reference": map[string]any{
					"type":        "string",
					"description": "for mode='ayah': surah:ayah like '2:255' (Ayat al-Kursi) or '112:1'",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "for mode='search': Arabic words from the verse",
				},
				"topic": map[string]any{
					"type":        "string",
					"description": "for mode='adhkar': morning, evening, tasbih, sleep, travel, healing, or faraj",
					"enum":        []string{"morning", "evening", "tasbih", "sleep", "travel", "healing", "faraj"},
				},
			},
			"required": []string{"mode"},
		},
		Access:  Read,
		Timeout: 7 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			mode := strings.ToLower(strings.TrimSpace(str(args["mode"])))
			switch mode {
			case "adhkar":
				topic := strings.ToLower(strings.TrimSpace(str(args["topic"])))
				if text, ok := builtInAdhkar[topic]; ok {
					return text, nil
				}
				return "الأذكار والأدعية الجاهزة عندي: أذكار الصباح، المساء، تسبيح الزهراء، النوم، السفر، الشفاء، ودعاء الفرج.", nil

			case "ayah":
				ref := strings.TrimSpace(str(args["reference"]))
				if ref == "" {
					return "", fmt.Errorf("حددي رقم السورة والآية مثل 2:255")
				}
				return fetchAyahWithTafsir(ctx, client, ref)

			case "search":
				q := strings.TrimSpace(str(args["query"]))
				if q == "" {
					return "", fmt.Errorf("اذكري كلمات من الآية للبحث")
				}
				return searchQuran(ctx, client, q)

			default:
				return "", fmt.Errorf("وضع غير معروف: %s", mode)
			}
		},
	}
}

func fetchAyahWithTafsir(ctx context.Context, client *http.Client, ref string) (string, error) {
	u := fmt.Sprintf("%s/ayah/%s/editions/quran-simple-clean,ar.muyassar", quranBase, url.PathEscape(ref))
	body, err := get(ctx, client, u, 64<<10, nil)
	if err != nil {
		return "", fmt.Errorf("ما قدرت أوصل لخدمة القرآن: %w", err)
	}
	var r struct {
		Code int `json:"code"`
		Data []struct {
			Text          string `json:"text"`
			NumberInSurah int    `json:"numberInSurah"`
			Surah         struct {
				Name string `json:"name"`
			} `json:"surah"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &r) != nil || r.Code != 200 || len(r.Data) == 0 {
		return "", fmt.Errorf("ما لقيت هالآية، تأكدي من رقم السورة والآية")
	}
	verse := r.Data[0]
	out := fmt.Sprintf("%s آية %d: %s", verse.Surah.Name, verse.NumberInSurah, oneLine(verse.Text, 300))
	if len(r.Data) > 1 && strings.TrimSpace(r.Data[1].Text) != "" {
		out += " — التفسير الميسّر: " + oneLine(r.Data[1].Text, 360)
	}
	return out, nil
}

func searchQuran(ctx context.Context, client *http.Client, query string) (string, error) {
	u := fmt.Sprintf("%s/search/%s/all/quran-simple-clean", quranBase, url.PathEscape(query))
	body, err := get(ctx, client, u, 64<<10, nil)
	if err != nil {
		return "", fmt.Errorf("ما قدرت أبحث في القرآن: %w", err)
	}
	var r struct {
		Code int `json:"code"`
		Data struct {
			Count   int `json:"count"`
			Matches []struct {
				Text          string `json:"text"`
				NumberInSurah int    `json:"numberInSurah"`
				Surah         struct {
					Number int    `json:"number"`
					Name   string `json:"name"`
				} `json:"surah"`
			} `json:"matches"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &r) != nil || r.Code != 200 || len(r.Data.Matches) == 0 {
		return "ما لقيت آية مطابقة لهذي الكلمات.", nil
	}
	m := r.Data.Matches[0]
	return fmt.Sprintf("أقرب نتيجة: %s آية %d (%d:%d): %s",
		m.Surah.Name, m.NumberInSurah, m.Surah.Number, m.NumberInSurah, oneLine(m.Text, 240)), nil
}

var builtInAdhkar = map[string]string{
	"morning": "من أذكار الصباح: «اللهم صلّ على محمد وآل محمد، اللهم يا من دلع لسان الصباح بنطق تبلّجه، بسم الله الذي لا يضر مع اسمه شيء في الأرض ولا في السماء وهو السميع العليم، يا حي يا قيوم برحمتك أستغيث أصلح لي شأني كله».",
	"evening": "من أذكار المساء: «اللهم صلّ على محمد وآل محمد، أمسينا وأمسى الملك لله والحمد لله لا إله إلا الله وحده لا شريك له، بسم الله الذي لا يضر مع اسمه شيء في الأرض ولا في السماء وهو السميع العليم، اللهم إني أسألك العفو والعافية في الدنيا والآخرة».",
	"tasbih":  "تسبيح السيدة فاطمة الزهراء عليها السلام: الله أكبر أربعاً وثلاثين مرة، ثم الحمد لله ثلاثاً وثلاثين مرة، ثم سبحان الله ثلاثاً وثلاثين مرة، ويُختم بالصلاة على محمد وآل محمد.",
	"sleep":   "من أذكار النوم: قراءة آية الكرسي وسور الإخلاص والفلق والناس، وتسبيح الزهراء عليها السلام، ثم «باسمك اللهم أموت وأحيا، اللهم قني عذابك يوم تبعث عبادك».",
	"travel":  "دعاء السفر: «اللهم صلّ على محمد وآل محمد، سبحان الذي سخّر لنا هذا وما كنا له مقرنين وإنا إلى ربنا لمنقلبون، اللهم إنا نسألك في سفرنا هذا البر والتقوى ومن العمل ما ترضى، اللهم هوّن علينا سفرنا هذا واطوِ عنا بعده».",
	"healing": "دعاء الشفاء والعافية: «اللهم صلّ على محمد وآل محمد، يا من اسمه دواء وذكره شفاء، اللهم اشفِ مرضانا ومرضى المؤمنين شفاءً عاجلاً لا يغادر سقماً، وألبسهم ثوب الصحة والعافية يا أرحم الراحمين».",
	"faraj":   "دعاء الفرج: «اللهم كن لوليّك الحجة بن الحسن صلواتك عليه وعلى آبائه في هذه الساعة وفي كل ساعة، ولياً وحافظاً وقائداً وناصراً ودليلاً وعيناً، حتى تسكنه أرضك طوعاً وتمتّعه فيها طويلاً، برحمتك يا أرحم الراحمين».",
}
