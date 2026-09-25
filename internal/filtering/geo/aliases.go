package geo

// countryAliases are the other names job postings use for a country, keyed by
// the folded name; the value is the alpha-2 code. The English name CLDR gives
// is registered separately and needs no entry here.
var countryAliases = map[string]string{
	"usa": "US", "us": "US", "united states of america": "US", "the united states": "US", "u s": "US", "america": "US",
	"uk": "GB", "united kingdom": "GB", "great britain": "GB", "britain": "GB", "england": "GB", "scotland": "GB",
	"wales": "GB", "northern ireland": "GB", "the uk": "GB", "gb": "GB",
	"uae": "AE", "united arab emirates": "AE", "the uae": "AE", "dubai": "AE", "abu dhabi": "AE",
	"south korea": "KR", "korea": "KR", "republic of korea": "KR", "north korea": "KP",
	"russian federation": "RU", "russia": "RU",
	"czech republic": "CZ", "czechia": "CZ",
	"turkey": "TR", "turkiye": "TR",
	"holland": "NL", "the netherlands": "NL", "netherlands": "NL",
	"vietnam": "VN", "viet nam": "VN",
	"ivory coast": "CI", "cote d ivoire": "CI", "cote divoire": "CI",
	"drc": "CD", "dr congo": "CD", "democratic republic of the congo": "CD", "democratic republic of congo": "CD", "congo kinshasa": "CD",
	"republic of the congo": "CG", "congo brazzaville": "CG",
	"hong kong": "HK", "hong kong sar": "HK", "macau": "MO", "macao": "MO",
	"taiwan": "TW", "palestine": "PS", "palestinian territories": "PS",
	"myanmar": "MM", "burma": "MM",
	"eswatini": "SZ", "swaziland": "SZ",
	"north macedonia": "MK", "macedonia": "MK",
	"cabo verde": "CV", "cape verde": "CV",
	"bosnia": "BA", "bosnia and herzegovina": "BA", "bosnia herzegovina": "BA",
	"trinidad and tobago": "TT", "trinidad": "TT",
	"laos": "LA", "syria": "SY", "iran": "IR", "bolivia": "BO", "venezuela": "VE", "tanzania": "TZ", "moldova": "MD",
	"brunei": "BN", "timor leste": "TL", "east timor": "TL", "micronesia": "FM", "vatican": "VA", "vatican city": "VA",
	"the gambia": "GM", "gambia": "GM", "the bahamas": "BS", "bahamas": "BS", "st lucia": "LC", "saint lucia": "LC",
	"st kitts and nevis": "KN", "antigua and barbuda": "AG", "sao tome and principe": "ST",
	"falkland islands": "FK", "channel islands": "JE", "puerto rico": "PR", "new zealand": "NZ", "aotearoa": "NZ",
	"the philippines": "PH", "philippines": "PH",
	"deutschland": "DE", "osterreich": "AT", "schweiz": "CH", "suisse": "CH", "svizzera": "CH", "espana": "ES", "italia": "IT",
	"polska": "PL", "ceska republika": "CZ", "magyarorszag": "HU", "nederland": "NL", "belgie": "BE", "belgique": "BE",
	"sverige": "SE", "norge": "NO", "danmark": "DK", "suomi": "FI", "eire": "IE", "brasil": "BR", "mexico": "MX", "turkiye cumhuriyeti": "TR",
	"ind": "IN", "cri": "CR", "sau": "SA", "ksa": "SA", "usa1": "US",
}

// extraCountries are places postings treat as countries that ISO/CLDR does not
// list as one.
var extraCountries = map[string]string{
	"kosovo": "XK",
}

// ambiguousNames are places whose name is also something else, so nothing is
// concluded from them.
var ambiguousNames = []string{"georgia"}

// worldwideNames say "anywhere". "International" and "remote" alone do not:
// they are not a statement about who may apply.
var worldwideNames = []string{
	"world wide", "global", "globally", "anywhere", "anywhere in the world", "anywhere in world", "anywhere on earth",
	"the world", "across the globe", "around the world", "all countries", "any country", "any location",
	"work from anywhere", "from anywhere", "worldwide remote", "global remote", "remote worldwide", "remote global", "remote anywhere",
}

type regionDef struct {
	names []string
	m49   []string // UN M.49 regions whose countries belong
	extra []string // further alpha-2 codes
}

var euMembers = []string{"AT", "BE", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "FR", "DE", "GR", "HU", "IE", "IT", "LV", "LT", "LU", "MT", "NL", "PL", "PT", "RO", "SK", "SI", "ES", "SE"}

var regionDefs = []regionDef{
	{names: []string{"africa", "african"}, m49: []string{"002"}},
	{names: []string{"sub saharan africa", "sub-saharan africa", "ssa"}, m49: []string{"202"}},
	{names: []string{"east africa", "eastern africa"}, m49: []string{"014"}},
	{names: []string{"west africa", "western africa"}, m49: []string{"011"}},
	{names: []string{"north africa", "northern africa"}, m49: []string{"015"}},
	{names: []string{"central africa", "middle africa"}, m49: []string{"017"}},
	{names: []string{"southern africa"}, m49: []string{"018"}},
	{names: []string{"europe", "european"}, m49: []string{"150"}},
	{names: []string{"european union", "eu"}, extra: euMembers},
	{names: []string{"eea", "european economic area"}, extra: append([]string{"IS", "LI", "NO"}, euMembers...)},
	{names: []string{"western europe"}, m49: []string{"155"}},
	{names: []string{"eastern europe", "central and eastern europe", "cee"}, m49: []string{"151"}},
	{names: []string{"northern europe"}, m49: []string{"154"}},
	{names: []string{"southern europe"}, m49: []string{"039"}},
	{names: []string{"uki", "uk and ireland", "uk i"}, extra: []string{"GB", "IE"}},
	{names: []string{"nordics", "nordic", "nordic countries"}, extra: []string{"DK", "FI", "IS", "NO", "SE"}},
	{names: []string{"scandinavia"}, extra: []string{"DK", "NO", "SE"}},
	{names: []string{"benelux"}, extra: []string{"BE", "NL", "LU"}},
	{names: []string{"dach"}, extra: []string{"DE", "AT", "CH"}},
	{names: []string{"baltics", "baltic states", "baltic"}, extra: []string{"EE", "LV", "LT"}},
	{names: []string{"emea"}, m49: []string{"150", "002", "145"}},
	{names: []string{"middle east", "mideast"}, m49: []string{"145"}},
	{names: []string{"mena", "middle east and north africa"}, m49: []string{"015", "145"}},
	{names: []string{"gcc", "gulf", "gulf states"}, extra: []string{"SA", "AE", "QA", "KW", "BH", "OM"}},
	{names: []string{"asia", "asian"}, m49: []string{"142"}},
	{names: []string{"apac", "asia pacific", "asia pac", "apj"}, m49: []string{"142", "009"}},
	{names: []string{"southeast asia", "south east asia", "sea", "asean"}, m49: []string{"035"}},
	{names: []string{"south asia", "southern asia"}, m49: []string{"034"}},
	{names: []string{"east asia", "eastern asia"}, m49: []string{"030"}},
	{names: []string{"oceania"}, m49: []string{"009"}},
	{names: []string{"anz"}, extra: []string{"AU", "NZ"}},
	{names: []string{"latam", "latin america", "lac", "latin america and the caribbean", "lat am"}, m49: []string{"419"}},
	{names: []string{"south america"}, m49: []string{"005"}},
	{names: []string{"central america"}, m49: []string{"013"}},
	{names: []string{"caribbean"}, m49: []string{"029"}},
	{names: []string{"north america", "northern america", "na", "nam"}, m49: []string{"021"}},
	{names: []string{"americas", "the americas", "amer"}, m49: []string{"019"}},
	{names: []string{"namer"}, m49: []string{"021"}},
}

func addRegions(byCode map[string]Place) {
	for _, d := range regionDefs {
		members := map[string]bool{}
		for code := range byCode {
			for _, m := range d.m49 {
				if member(m, code) {
					members[code] = true
				}
			}
		}
		for _, c := range d.extra {
			if _, ok := byCode[c]; ok {
				members[c] = true
			}
		}
		key := d.names[0]
		display := titleCase(key)
		switch key {
		case "emea", "apac", "mena", "gcc", "dach", "cee", "eea", "anz", "latam":
			display = upper(key)
		case "european union":
			display = "European Union"
		}
		p := Place{Name: display, Kind: Region, Code: key, countries: members}
		for _, n := range d.names {
			add(n, p)
		}
	}
}

func titleCase(s string) string {
	out := []rune(s)
	up := true
	for i, r := range out {
		if up && r >= 'a' && r <= 'z' {
			out[i] = r - 32
		}
		up = r == ' ' || r == '-'
	}
	return string(out)
}

func upper(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'a' && r <= 'z' {
			out[i] = r - 32
		}
	}
	return string(out)
}

// subdivisions: name -> alpha-2 of the country they are in.
var subdivisions = map[string]string{}

func init() {
	for _, s := range []string{"alabama", "alaska", "arizona", "arkansas", "california", "colorado", "connecticut", "delaware",
		"florida", "hawaii", "idaho", "illinois", "indiana", "iowa", "kansas", "kentucky", "louisiana", "maine", "maryland",
		"massachusetts", "michigan", "minnesota", "mississippi", "missouri", "montana", "nebraska", "nevada", "new hampshire",
		"new jersey", "new mexico", "new york", "north carolina", "north dakota", "ohio", "oklahoma", "oregon", "pennsylvania",
		"rhode island", "south carolina", "south dakota", "tennessee", "texas", "utah", "vermont", "virginia", "washington",
		"west virginia", "wisconsin", "wyoming", "district of columbia", "washington dc", "washington d c", "silicon valley",
		"northeast", "southeast", "midwest", "west coast", "east coast", "southwest", "pacific northwest", "mid atlantic", "new england",
		"tri state", "south texas", "north texas", "east texas", "west texas", "southern california", "northern california", "central texas"} {
		subdivisions[s] = "US"
	}
	for _, s := range []string{"alberta", "british columbia", "manitoba", "new brunswick", "newfoundland and labrador", "newfoundland",
		"nova scotia", "ontario", "quebec", "prince edward island", "saskatchewan", "yukon", "northwest territories", "nunavut"} {
		subdivisions[s] = "CA"
	}
	for _, s := range []string{"new south wales", "queensland", "tasmania", "western australia", "south australia",
		"australian capital territory", "northern territory"} {
		subdivisions[s] = "AU"
	}
	// Ethiopia's regions and chartered cities (the Ethiopian job boards use them as locations).
	for _, s := range []string{"addis ababa", "addis abeba", "dire dawa", "oromia", "amhara", "tigray", "afar", "somali region", "sidama",
		"benishangul gumuz", "benishangul", "gambela", "gambella", "harari", "south west ethiopia", "snnpr", "southern nations",
		"adama", "nazret", "hawassa", "awassa", "bahir dar", "mekelle", "mekele", "jimma", "gondar", "gonder", "dessie", "jijiga", "bishoftu", "debre zeit", "debre markos", "arba minch", "harar", "semera"} {
		subdivisions[s] = "ET"
	}
}

func addSubdivisions() {
	for name, code := range subdivisions {
		add(name, Place{Name: titleCase(name), Kind: Subdivision, Code: code})
	}
	for name, code := range cities {
		add(name, Place{Name: titleCase(name), Kind: Subdivision, Code: code, City: true})
	}
}
