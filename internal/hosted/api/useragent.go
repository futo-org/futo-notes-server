package api

import "strings"

// uaMarker maps a User-Agent substring to the name it stands for. Order
// matters: browsers claim each other's tokens (every Chromium browser says
// "Safari", Edge and Opera say "Chrome"), and iOS browsers say "Mac OS X".
type uaMarker struct{ token, name string }

var (
	uaBrowsers = []uaMarker{
		{"Edg/", "Edge"}, {"EdgA/", "Edge"}, {"EdgiOS/", "Edge"},
		{"OPR/", "Opera"},
		{"SamsungBrowser/", "Samsung Internet"},
		{"Firefox/", "Firefox"}, {"FxiOS/", "Firefox"},
		{"CriOS/", "Chrome"}, {"Chrome/", "Chrome"}, {"Chromium/", "Chromium"},
		{"Version/", "Safari"},
	}
	uaSystems = []uaMarker{
		{"iPhone", "iPhone"}, {"iPad", "iPad"},
		{"Android", "Android"},
		{"CrOS", "ChromeOS"},
		{"Windows", "Windows"},
		{"Macintosh", "macOS"},
		{"Linux", "Linux"},
	}
)

func uaFind(ua string, markers []uaMarker) string {
	for _, m := range markers {
		if strings.Contains(ua, m.token) {
			return m.name
		}
	}
	return ""
}

// deviceLabel names a signed-in device for people: the name its app gave,
// else a short reading of its User-Agent such as "Chrome on macOS", else the
// User-Agent's first product name.
func deviceLabel(deviceName, ua string) string {
	if deviceName != "" {
		return deviceName
	}
	browser, system := uaFind(ua, uaBrowsers), uaFind(ua, uaSystems)
	if browser == "Safari" && !strings.Contains(ua, "Safari/") {
		browser = ""
	}
	switch {
	case browser != "" && system != "":
		return browser + " on " + system
	case browser != "":
		return browser
	case system != "":
		return system
	}
	product, _, _ := strings.Cut(strings.TrimSpace(ua), " ")
	product, _, _ = strings.Cut(product, "/")
	if product == "" {
		return "Unknown device"
	}
	return product
}
