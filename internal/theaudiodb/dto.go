package theaudiodb

// Wire structs for TheAudioDB. Only the fields Spotistats uses are declared.
//
// Every value TheAudioDB returns is a STRING, including the numeric ones ("1996", "6"), and
// absent values arrive as null or "". Nothing here is typed as an int for that reason: a
// json.Number or int would fail to decode on a value the service happily sends.

// dtoArtistResponse wraps the single-element array TheAudioDB returns.
//
// Artists is nil (JSON null) rather than an empty array when the MBID is unknown, so a nil
// check is the "not found" signal.
type dtoArtistResponse struct {
	Artists []dtoArtist `json:"artists"`
}

// dtoArtist is one artist record.
type dtoArtist struct {
	ID            string `json:"idArtist"`
	Name          string `json:"strArtist"`
	MusicBrainzID string `json:"strMusicBrainzID"`

	// Biography holds the ENGLISH text. The suffixed fields are translations -- and
	// strBiographyEN is present but EMPTY, so English must be read from here. A selector
	// written the obvious way ("strBiography" + upper(lang)) returns nothing for English.
	Biography   string `json:"strBiography"`
	BiographyDE string `json:"strBiographyDE"`
	BiographyES string `json:"strBiographyES"`
	BiographyFR string `json:"strBiographyFR"`
	BiographyIT string `json:"strBiographyIT"`
	BiographyPT string `json:"strBiographyPT"`
	BiographyNL string `json:"strBiographyNL"`
	BiographySE string `json:"strBiographySE"`
	BiographyNO string `json:"strBiographyNO"`
	BiographyRU string `json:"strBiographyRU"`
	BiographyPL string `json:"strBiographyPL"`
	BiographyJP string `json:"strBiographyJP"`
	BiographyCN string `json:"strBiographyCN"`
	BiographyHU string `json:"strBiographyHU"`

	Thumb     string `json:"strArtistThumb"`
	Logo      string `json:"strArtistLogo"`
	Cutout    string `json:"strArtistCutout"`
	ClearArt  string `json:"strArtistClearart"`
	WideThumb string `json:"strArtistWideThumb"`
	Banner    string `json:"strArtistBanner"`
	Fanart    string `json:"strArtistFanart"`
	Fanart2   string `json:"strArtistFanart2"`
	Fanart3   string `json:"strArtistFanart3"`
	Fanart4   string `json:"strArtistFanart4"`

	// These structured fields are read only to be IGNORED in favour of MusicBrainz, and are
	// declared so the reason is visible rather than rediscovered:
	//   FormedYear disagrees with MusicBrainz and sometimes with this record's own biography.
	//   Country is free text ("Waddinxveen, Netherlands") where MusicBrainz gives NL + a city.
	//   Members is a COUNT with no names.
	// See docs/SPECS.md 4.5.1.
	FormedYear string `json:"intFormedYear"`
	Country    string `json:"strCountry"`
	Members    string `json:"intMembers"`

	// CreativeCommons marks artwork that is freely reusable. Where absent, the image is
	// "displayable with credit" rather than free -- see docs/SPECS.md 4.5.7.
	CreativeCommons string `json:"strCreativeCommons"`
}
