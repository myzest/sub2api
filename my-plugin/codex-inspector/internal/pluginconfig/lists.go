package pluginconfig

// Whitelist is the configured IANA timezone allowlist.
var Whitelist = []string{
	"Africa/Cairo", "Africa/Johannesburg", "America/Argentina/Buenos_Aires",
	"America/Chicago", "America/Denver", "America/Los_Angeles", "America/Mexico_City",
	"America/New_York", "America/Sao_Paulo", "America/Toronto", "America/Vancouver",
	"Asia/Bangkok", "Asia/Dubai", "Asia/Ho_Chi_Minh", "Asia/Jakarta", "Asia/Kolkata",
	"Asia/Kuala_Lumpur", "Asia/Manila", "Asia/Seoul", "Asia/Singapore", "Asia/Tokyo",
	"Australia/Perth", "Australia/Sydney", "Europe/Berlin", "Europe/Istanbul",
	"Europe/London", "Europe/Moscow", "Europe/Paris", "Pacific/Auckland", "Pacific/Honolulu",
}

var SelectableModels = []string{
	"gpt-5.4", "gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
	"gpt-6-astra", "gpt-6-sol", "gpt-6-luna",
}

func InWhitelist(tz string) bool { return contains(Whitelist, tz) }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
