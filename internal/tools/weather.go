package tools

// The weather, without searching for it.
//
// "كم درجة الحرارة عندنا الحين" is the question he asks most, and answering it through a general web
// search is the wrong shape three times over: it takes 3.7 seconds, it returns a page ABOUT the
// weather rather than the weather, and the model then reads a number off an advert.
//
// His location does not move. So this asks one free, keyless service for the reading at his own
// coordinates and returns the numbers. Measured from the bridge's own machine: 445-512 ms across
// three calls, against 3744 ms for the search it replaces.
//
// There is no API key because there is no account. open-meteo serves this openly, and the only
// thing that leaves the house is a pair of coordinates that are already in the configuration.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// weatherBase is a variable so a test can point it at a local server.
var weatherBase = "https://api.open-meteo.com/v1/forecast"

// wmo turns the World Meteorological Organization's code into words. Only the codes that actually
// occur on this coast are spelled out; anything else falls through to a shrug rather than a lie.
func wmo(code int) string {
	switch {
	case code == 0:
		return "clear"
	case code <= 2:
		return "partly cloudy"
	case code == 3:
		return "overcast"
	case code == 45 || code == 48:
		return "foggy"
	case code >= 51 && code <= 57:
		return "drizzling"
	case code >= 61 && code <= 67:
		return "raining"
	case code >= 80 && code <= 82:
		return "rain showers"
	case code >= 95:
		return "thunderstorms"
	default:
		return ""
	}
}

// Weather reports the current conditions at one fixed place.
func Weather(lat, lon float64, tz string, client *http.Client) *Tool {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &Tool{
		Name: "weather_now",
		Description: "The weather right now where he lives: temperature, what it feels like, " +
			"humidity and wind. Answers 'كم درجة الحرارة', 'كيف الجو', 'الجو حار اليوم', " +
			"'how hot is it', 'what is the weather'. Use this instead of searching the web; it is " +
			"the reading itself, and it is far faster.",
		Access:  Read,
		Ambient: true,
		Timeout: 6 * time.Second,
		Run: func(ctx context.Context, _ map[string]any) (string, error) {
			u := fmt.Sprintf("%s?latitude=%.4f&longitude=%.4f&timezone=%s"+
				"&current=temperature_2m,relative_humidity_2m,apparent_temperature,wind_speed_10m,weather_code",
				weatherBase, lat, lon, url.QueryEscape(tz))
			body, err := get(ctx, client, u, 64<<10, nil)
			if err != nil {
				return "", fmt.Errorf("the weather service did not answer: %w", err)
			}
			var r struct {
				Current struct {
					Time     string  `json:"time"`
					Temp     float64 `json:"temperature_2m"`
					Feels    float64 `json:"apparent_temperature"`
					Humidity float64 `json:"relative_humidity_2m"`
					Wind     float64 `json:"wind_speed_10m"`
					Code     int     `json:"weather_code"`
				} `json:"current"`
			}
			if json.Unmarshal(body, &r) != nil || r.Current.Time == "" {
				return "", fmt.Errorf("the weather service sent something unreadable")
			}
			c := r.Current

			// The feels-like figure is the one that matters on this coast: 34 degrees at 69 percent
			// humidity is 42 to a person standing in it. Say it only when it differs enough to be
			// worth a second number.
			s := fmt.Sprintf("%.0f degrees", c.Temp)
			if diff := c.Feels - c.Temp; diff >= 2 || diff <= -2 {
				s += fmt.Sprintf(", feels like %.0f", c.Feels)
			}
			if w := wmo(c.Code); w != "" {
				s += ", " + w
			}
			s += fmt.Sprintf(", humidity %.0f percent, wind %.0f kilometres an hour", c.Humidity, c.Wind)
			return s + ".", nil
		},
	}
}
