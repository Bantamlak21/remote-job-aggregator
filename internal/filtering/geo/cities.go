package geo

// cities maps cities that job postings name as a location (offices, hubs) to
// their country. It is deliberately a list of large cities and tech hubs, not
// a gazetteer: a city only matters for saying where an office is, and a name
// that is also a common word or a place in several countries is left out
// ("Cambridge", "Springfield", "Reading", "Birmingham"). Keys are folded.
var cities = map[string]string{
	// United States
	"san francisco": "US", "sf": "US", "bay area": "US", "san francisco bay area": "US", "new york city": "US", "nyc": "US", "brooklyn": "US",
	"los angeles": "US", "santa monica": "US", "san diego": "US", "san jose": "US", "palo alto": "US", "mountain view": "US",
	"menlo park": "US", "redwood city": "US", "sunnyvale": "US", "santa clara": "US", "foster city": "US", "oakland": "US",
	"cupertino": "US", "south san francisco": "US", "seattle": "US", "bellevue": "US", "redmond": "US", "portland": "US", "boston": "US",
	"cambridge ma": "US", "chicago": "US", "austin": "US", "dallas": "US", "houston": "US", "denver": "US", "boulder": "US",
	"atlanta": "US", "miami": "US", "phoenix": "US", "salt lake city": "US", "las vegas": "US", "philadelphia": "US",
	"pittsburgh": "US", "raleigh": "US", "durham": "US", "charlotte": "US", "nashville": "US", "minneapolis": "US", "detroit": "US",
	"columbus": "US", "cleveland": "US", "st louis": "US", "kansas city": "US", "baltimore": "US", "arlington": "US", "reston": "US",
	"mclean": "US", "tampa": "US", "orlando": "US", "san antonio": "US", "sacramento": "US", "irvine": "US", "el segundo": "US",
	"culver city": "US", "burlingame": "US", "san mateo": "US", "pleasanton": "US", "fremont": "US", "hoboken": "US", "stamford": "US",
	// Canada
	"toronto": "CA", "vancouver": "CA", "montreal": "CA", "ottawa": "CA", "calgary": "CA", "edmonton": "CA", "waterloo": "CA",
	"kitchener": "CA", "victoria bc": "CA", "halifax": "CA", "winnipeg": "CA", "mississauga": "CA",
	// Mexico, Central and South America, Caribbean
	"mexico city": "MX", "ciudad de mexico": "MX", "guadalajara": "MX", "monterrey": "MX", "queretaro": "MX", "tijuana": "MX",
	"sao paulo": "BR", "rio de janeiro": "BR", "brasilia": "BR", "belo horizonte": "BR", "curitiba": "BR", "porto alegre": "BR", "florianopolis": "BR",
	"buenos aires": "AR", "cordoba ar": "AR", "santiago": "CL", "bogota": "CO", "medellin": "CO", "lima": "PE", "montevideo": "UY",
	"quito": "EC", "panama city": "PA", "san jose costa rica": "CR", "san juan": "PR", "kingston": "JM", "santo domingo": "DO",
	// United Kingdom and Ireland
	"london": "GB", "manchester": "GB", "edinburgh": "GB", "glasgow": "GB", "bristol": "GB", "leeds": "GB", "belfast": "GB", "oxford": "GB",
	"cardiff": "GB", "nottingham": "GB", "sheffield": "GB", "liverpool": "GB", "dublin": "IE", "cork": "IE", "galway": "IE",
	// Western Europe
	"paris": "FR", "lyon": "FR", "marseille": "FR", "toulouse": "FR", "nantes": "FR", "lille": "FR", "bordeaux": "FR",
	"berlin": "DE", "munich": "DE", "hamburg": "DE", "frankfurt": "DE", "cologne": "DE", "koln": "DE", "stuttgart": "DE", "dusseldorf": "DE", "leipzig": "DE", "dresden": "DE",
	"amsterdam": "NL", "rotterdam": "NL", "the hague": "NL", "utrecht": "NL", "eindhoven": "NL", "brussels": "BE", "antwerp": "BE", "ghent": "BE",
	"luxembourg city": "LU", "zurich": "CH", "geneva": "CH", "basel": "CH", "lausanne": "CH", "vienna": "AT", "salzburg": "AT",
	"madrid": "ES", "barcelona": "ES", "valencia": "ES", "seville": "ES", "malaga": "ES", "bilbao": "ES",
	"lisbon": "PT", "porto": "PT", "milan": "IT", "rome": "IT", "turin": "IT", "bologna": "IT", "florence": "IT", "naples": "IT",
	"athens": "GR", "thessaloniki": "GR", "valletta": "MT",
	// Northern and Eastern Europe
	"stockholm": "SE", "gothenburg": "SE", "malmo": "SE", "copenhagen": "DK", "aarhus": "DK", "oslo": "NO", "bergen": "NO",
	"helsinki": "FI", "espoo": "FI", "tampere": "FI", "reykjavik": "IS", "tallinn": "EE", "tartu": "EE", "riga": "LV", "vilnius": "LT", "kaunas": "LT",
	"warsaw": "PL", "krakow": "PL", "wroclaw": "PL", "gdansk": "PL", "poznan": "PL", "lodz": "PL", "prague": "CZ", "brno": "CZ",
	"bratislava": "SK", "budapest": "HU", "bucharest": "RO", "cluj napoca": "RO", "cluj": "RO", "sofia": "BG", "belgrade": "RS", "zagreb": "HR",
	"ljubljana": "SI", "sarajevo": "BA", "skopje": "MK", "tirana": "AL", "kyiv": "UA", "kiev": "UA", "lviv": "UA", "kharkiv": "UA", "odesa": "UA",
	"dnipro": "UA", "minsk": "BY", "chisinau": "MD", "tbilisi": "GE", "yerevan": "AM", "baku": "AZ", "moscow": "RU", "saint petersburg": "RU",
	"st petersburg": "RU", "novosibirsk": "RU", "istanbul": "TR", "ankara": "TR", "izmir": "TR", "limassol": "CY", "nicosia": "CY",
	// Middle East
	"tel aviv": "IL", "jerusalem": "IL", "haifa": "IL", "herzliya": "IL", "dubai": "AE", "abu dhabi": "AE", "riyadh": "SA", "jeddah": "SA",
	"doha": "QA", "manama": "BH", "kuwait city": "KW", "muscat": "OM", "amman": "JO", "beirut": "LB", "baghdad": "IQ", "tehran": "IR",
	// Africa
	"cairo": "EG", "alexandria": "EG", "lagos": "NG", "abuja": "NG", "nairobi": "KE", "mombasa": "KE", "kampala": "UG", "kigali": "RW",
	"dar es salaam": "TZ", "arusha": "TZ", "accra": "GH", "kumasi": "GH", "dakar": "SN", "abidjan": "CI", "casablanca": "MA", "rabat": "MA",
	"marrakech": "MA", "tunis": "TN", "algiers": "DZ", "cape town": "ZA", "johannesburg": "ZA", "pretoria": "ZA", "durban": "ZA",
	"harare": "ZW", "lusaka": "ZM", "gaborone": "BW", "windhoek": "NA", "maputo": "MZ", "luanda": "AO", "kinshasa": "CD", "douala": "CM",
	"djibouti": "DJ", "asmara": "ER", "mogadishu": "SO", "khartoum": "SD", "juba": "SS", "port louis": "MU",
	// South and Southeast Asia
	"bangalore": "IN", "bengaluru": "IN", "mumbai": "IN", "delhi": "IN", "new delhi": "IN", "gurgaon": "IN", "gurugram": "IN", "noida": "IN",
	"hyderabad": "IN", "chennai": "IN", "pune": "IN", "kolkata": "IN", "ahmedabad": "IN", "kochi": "IN", "jaipur": "IN", "chandigarh": "IN",
	"karachi": "PK", "lahore": "PK", "islamabad": "PK", "dhaka": "BD", "colombo": "LK", "kathmandu": "NP",
	"singapore": "SG", "kuala lumpur": "MY", "penang": "MY", "jakarta": "ID", "bali": "ID", "surabaya": "ID", "bangkok": "TH", "chiang mai": "TH",
	"ho chi minh city": "VN", "saigon": "VN", "hanoi": "VN", "da nang": "VN", "manila": "PH", "makati": "PH", "cebu": "PH", "quezon city": "PH",
	"phnom penh": "KH", "yangon": "MM",
	// East Asia and Oceania
	"tokyo": "JP", "osaka": "JP", "kyoto": "JP", "yokohama": "JP", "seoul": "KR", "busan": "KR", "beijing": "CN", "shanghai": "CN",
	"shenzhen": "CN", "guangzhou": "CN", "hangzhou": "CN", "chengdu": "CN", "taipei": "TW", "hsinchu": "TW", "taichung": "TW",
	"sydney": "AU", "melbourne": "AU", "brisbane": "AU", "perth": "AU", "adelaide": "AU", "canberra": "AU",
	"warszawa": "PL", "praha": "CZ", "wien": "AT", "roma": "IT", "milano": "IT", "lisboa": "PT", "bruxelles": "BE", "koeln": "DE",
	"muenchen": "DE", "cdmx": "MX", "budapest hu": "HU", "gyor": "HU", "pecs": "HU", "szeged": "HU", "debrecen": "HU", "rijeka": "HR", "split": "HR",
	"pula": "HR", "sibenik": "HR", "pisa": "IT", "brighton": "GB", "derby": "GB", "peterborough": "GB", "blackpool": "GB", "braga": "PT",
	"phuket": "TH", "mohali": "IN", "escazu": "CR", "tysons": "US", "cincinnati": "US", "indianapolis": "US", "milwaukee": "US",
	"oklahoma city": "US", "louisville": "US", "memphis": "US", "richmond": "US", "buffalo": "US", "rochester": "US", "albany": "US",
	"albuquerque": "US", "tucson": "US", "omaha": "US", "boise": "US", "honolulu": "US", "anchorage": "US", "new orleans": "US",
	"jacksonville": "US", "fort worth": "US", "el paso": "US", "fresno": "US", "long beach": "US", "colorado springs": "US",
	"auckland": "NZ", "wellington": "NZ", "christchurch": "NZ", "almaty": "KZ", "astana": "KZ", "tashkent": "UZ",
}
