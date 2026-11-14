package onboarding

// iccProfiles are the ICC colour profiles whose colours are judged as the device space of their
// component count (WIS-625 post-merge review): a profile is code, and a made-up one can show
// black as white, so only these exact standard profiles are trusted, by the SHA-256 of the
// decoded profile stream, with the components each must declare (/N). Each hash was taken from
// the file named, as shipped by the Ubuntu package given (verified 2026-11-08):
//
//	eddaf344b5edea13269e0d20055f335610e5e0b6e33e6e536f2701bc18c5f7d5  Artifex sRGB (srgb.icc, default_rgb.icc)      libgs-common 10.02.1~dfsg1-0ubuntu7.10
//	0976fe7bc0f118a47c89747cb707c6f76044b48baf670dad811d0514bc2c4214  Artifex sGray (sgray.icc)                      libgs-common 10.02.1~dfsg1-0ubuntu7.10
//	d3aaaa8ad45ec8a0108fd393a07488073ee24561fd5ed2d1f88c59771278cabd  Artifex sGray (default_gray.icc)               libgs-common 10.02.1~dfsg1-0ubuntu7.10
//	8472fa1493a024b800b67dee9424835ec0c41ab79490200ae8ec4a689fd1b9a9  Artifex CMYK SWOP (default_cmyk.icc)           libgs-common 10.02.1~dfsg1-0ubuntu7.10
//	a48304557987a1eb9f23137b4e53cbdc46fdc444827248a1465252f375775460  sRGB (colord sRGB.icc)                         colord-data 1.4.7-1build2
//	1f91b4e398bc3b899e54aa63eb894f1691f862f69706970d453deb96d301fe00  SWOP TR003 Coated (SWOP_TR003_coated_3.icc)    colord-data 1.4.7-1build2
//
// and from the ICC's own registry over HTTPS (https://registry.color.org/rgb-registry/profiles/),
// each also checked against the MD5 profile ID in its header (ICC.1 7.2.18), 2026-11-08:
//
//	384b832de3412066743b52a75ee906b6fb9fb8d9e09e936fc2c43223815c6e0a  ICC sRGB2014.icc (v2, "sRGB IEC61966-2.1", 3024 bytes)
//	83174717332326ddc198d9df188a4daec27b8979ba152cebbfc470c793d0bb11  ICC sRGB_v4_ICC_preference.icc (v4, 60960 bytes)
//
// HP's original "sRGB IEC61966-2.1" (3144 bytes), Adobe's "U.S. Web Coated (SWOP) v2" and the
// "Coated FOGRA39" profiles, which office and print software also embed, are not listed: no copy
// from their publishers could be fetched and verified here. Each is added by its reviewed hash.
var iccProfiles = map[string]int64{
	"eddaf344b5edea13269e0d20055f335610e5e0b6e33e6e536f2701bc18c5f7d5": 3,
	"0976fe7bc0f118a47c89747cb707c6f76044b48baf670dad811d0514bc2c4214": 1,
	"d3aaaa8ad45ec8a0108fd393a07488073ee24561fd5ed2d1f88c59771278cabd": 1,
	"8472fa1493a024b800b67dee9424835ec0c41ab79490200ae8ec4a689fd1b9a9": 4,
	"a48304557987a1eb9f23137b4e53cbdc46fdc444827248a1465252f375775460": 3,
	"1f91b4e398bc3b899e54aa63eb894f1691f862f69706970d453deb96d301fe00": 4,
	"384b832de3412066743b52a75ee906b6fb9fb8d9e09e936fc2c43223815c6e0a": 3,
	"83174717332326ddc198d9df188a4daec27b8979ba152cebbfc470c793d0bb11": 3,
}

// maxICCProfile bounds the profile stream read to hash it.
const maxICCProfile = 1 << 20 // the listed profiles are under 200 KiB
