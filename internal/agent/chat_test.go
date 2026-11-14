package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
)

const (
	chatRun           = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"
	chatSource        = "0b0b0b0b-0b0b-4b0b-8b0b-0b0b0b0b0b0b"
	spanSOP           = "0c0c0c0c-0c0c-4c0c-8c0c-0c0c0c0c0c0c"
	spanNote          = "0d0d0d0d-0d0d-4d0d-8d0d-0d0d0d0d0d0d"
	spanUnread        = "0e0e0e0e-0e0e-4e0e-8e0e-0e0e0e0e0e0e"
	spanApprovedPlain = "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f"
	spanUnapproved30  = "10101010-1010-4010-8010-101010101010"
	span250           = "11111111-1111-4111-8111-111111111111"
	spanHyphens       = "12121212-1212-4212-8212-121212121212"
	spanCycle45       = "13131313-1313-4313-8313-131313131313"
	spanCycleDot45    = "14141414-1414-4414-8414-141414141414"
	spanCycle045      = "15151515-1515-4515-8515-151515151515"
	spanRevLabel      = "16161616-1616-4616-8616-161616161616"
	spanPunct         = "17171717-1717-4717-8717-171717171717"
	spanRange0        = "18181818-1818-4818-8818-181818181818"
	spanRangeDot      = "19191919-1919-4919-8919-191919191919"
	spanRangeSpaced   = "1a1a1a1a-1a1a-4a1a-8a1a-1a1a1a1a1a1a"
	spanExponent      = "1b1b1b1b-1b1b-4b1b-8b1b-1b1b1b1b1b1b"
	spanGluedTaint    = "1c1c1c1c-1c1c-4c1c-8c1c-1c1c1c1c1c1c"
	spanFillerExp     = "1d1d1d1d-1d1d-4d1d-8d1d-1d1d1d1d1d1d"
	spanFillerGap     = "1e1e1e1e-1e1e-4e1e-8e1e-1e1e1e1e1e1e"
	spanIdentExp      = "1f1f1f1f-1f1f-4f1f-8f1f-1f1f1f1f1f1f"
	spanLabelExp      = "2a2a2a2a-2a2a-4a2a-8a2a-2a2a2a2a2a2a"
)

// WIS-517: the answer contract is enforced deterministically on hostile model output (rule 08).
func TestValidateChatOutput(t *testing.T) {
	child := DelegationEnvelopeV1{RunID: chatRun, Role: RoleDiscoveryMapping}
	spanSource := map[string]string{spanSOP: chatSource, spanNote: chatSource, spanApprovedPlain: chatSource, spanUnapproved30: chatSource, span250: chatSource, spanHyphens: chatSource,
		spanCycle45: chatSource, spanCycleDot45: chatSource, spanCycle045: chatSource, spanRevLabel: chatSource, spanPunct: chatSource, spanRange0: chatSource, spanRangeDot: chatSource, spanRangeSpaced: chatSource, spanExponent: chatSource, spanGluedTaint: chatSource, spanFillerExp: chatSource, spanFillerGap: chatSource, spanIdentExp: chatSource, spanLabelExp: chatSource}
	sources := map[string]sourceRead{chatSource: {revision: 2,
		spans: map[string]string{
			spanSOP:  "Torque the infeed bolts to 25 Nm. Set offset to -1,250.75 mm. Gap 4\u00a0mm.",
			spanNote: "Inspect station 3 every shift. Use M4E16.", spanApprovedPlain: "Bolts must be clean.",
			spanUnapproved30: "Retorque every 30 min. Offset is \u221240 mm.", span250: "Rated for 250 Nm.",
			spanHyphens: "Inspect Line-2 hourly per SOP-104; torque 20-30 Nm at station 10; clearance 5 - 8 mm.",
			spanCycle45: "Cycle is 45 seconds a part.", spanCycleDot45: "Cycle is .45 seconds a part.", spanCycle045: "Cycle is 0.45 seconds a part.",
			spanFillerGap: "Cycle .45e\u3164-2 or 55e\u200b2 s. Dwell .65e\ufe0f2 or .75e\u034f2 s. Hold .35e\ufb292, .55e\uff12, .85e\u00b2, .95\ufe0fe7 and .15\u3164e9 s.", spanIdentExp: "Use M4E16.45e2 now, then M4E16e2.",
			spanLabelExp:    "Rack 3E, 25 parts. Grade 3e. 40 trays; bin 7e: 12 kg. Shelf 5E = 30 bins.",
			spanFillerExp:   "Cycle .45e\u207b2, .45e 2, .45e\uff0d2, .45e\u20142 and \u316445e2 s.",
			spanGluedTaint:  "Use 1e5-7 per Rev.3/4 and v.2:5. Cycle .45e\u22122 or .55e+ 3 s. Follow SOP.A-999/6, SOP.B-998x8 and SOP.C-997:9.",
			spanRangeSpaced: "Torque .45 - .60 Nm.", spanExponent: "Cycle .45e2 seconds. Pack 45 each, grade 3e. Also 45.e2, 4.E+1 and 45.E-1.",
			spanRange0: "Torque 0.45-0.60 Nm.", spanRangeDot: "Torque .45-.60 Nm. Per Rev.3-4.",
			spanPunct:    "Set a=.3 then x:.3, #.3 and /.3.",
			spanRevLabel: "Per Rev.3 and v.2 of No.5, steps 1.2.3 and 1..7 apply.",
		}}}
	approved := map[string]bool{spanSOP: true, spanApprovedPlain: true, span250: true}
	answer := func(status string, statements []ChatStatement, missing []string) ChatAnswerOutputV2 {
		return ChatAnswerOutputV2{SchemaVersion: ChatSchemaVersion, RunID: chatRun, Role: RoleDiscoveryMapping, Status: status, Statements: statements, MissingInputs: missing}
	}
	fact := func(text string, spans ...string) ChatStatement {
		return ChatStatement{Kind: "approved_fact", Text: text, SourceSpanIDs: spans}
	}
	for name, c := range map[string]struct {
		out  ChatAnswerOutputV2
		want string
	}{
		"cited approved fact with a sourced number": {answer("answered", []ChatStatement{fact("Infeed bolts are torqued to 25 Nm.", spanSOP)}, []string{}), ""},
		"signed decimal token in cited span":        {answer("answered", []ChatStatement{fact("Set the offset to -1,250.75 mm.", spanSOP)}, []string{}), ""},
		"number sign is part of the token":          {answer("answered", []ChatStatement{fact("Set the offset to +1,250.75 mm.", spanSOP)}, []string{}), CodeNumberNotSourced},
		"observation citing a read span":            {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Station 3 is inspected each shift.", SourceSpanIDs: []string{spanNote}}}, []string{}), ""},
		"needs input with missing inputs":           {answer("needs_input", []ChatStatement{}, []string{"Shift plan for line 2"}), ""},
		"number not in any cited source":            {answer("answered", []ChatStatement{fact("Infeed bolts are torqued to 99 Nm.", spanSOP)}, []string{}), CodeNumberNotSourced},
		"5 is not sourced by 25":                    {answer("answered", []ChatStatement{fact("Infeed bolts are torqued to 5 Nm.", spanSOP)}, []string{}), CodeNumberNotSourced},
		"25 is not sourced by 250":                  {answer("answered", []ChatStatement{fact("Rated for 25 Nm.", span250)}, []string{}), CodeNumberNotSourced},
		// #192 review: a hyphen inside an identifier or range is not a minus sign.
		"hyphenated identifier is not negative":     {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Line 2 is inspected hourly.", SourceSpanIDs: []string{spanHyphens}}}, []string{}), ""},
		"document code is not negative":             {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Follow SOP 104.", SourceSpanIDs: []string{spanHyphens}}}, []string{}), ""},
		"range bound is not negative":               {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Torque between 20 and 30 Nm.", SourceSpanIDs: []string{spanHyphens}}}, []string{}), ""},
		"hyphenated identifier restated as written": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Line-2 is inspected hourly per SOP-104.", SourceSpanIDs: []string{spanHyphens}}}, []string{}), ""},
		"range restated as written":                 {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Torque to 20-30 Nm.", SourceSpanIDs: []string{spanHyphens}}}, []string{}), ""},
		"glued minus the source never had":          {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Torque to-30 Nm.", SourceSpanIDs: []string{spanHyphens}}}, []string{}), CodeNumberNotSourced},
		// #192 re-review: what a reader sees must be what the ASCII tokens check.
		"unicode minus sign":                    {answer("answered", []ChatStatement{fact("Torque the bolts to \u221225 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"en dash as minus":                      {answer("answered", []ChatStatement{fact("Torque the bolts to \u201325 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"zero-width space after sign":           {answer("answered", []ChatStatement{fact("Torque the bolts to -\u200b25 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"full-width digits":                     {answer("answered", []ChatStatement{fact("Torque the bolts to \uff19\uff19 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"arabic-indic digits":                   {answer("answered", []ChatStatement{fact("Torque the bolts to \u0669\u0669 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"spaced sign is a sign":                 {answer("answered", []ChatStatement{fact("Torque the bolts to - 25 Nm.", spanSOP)}, []string{}), CodeNumberNotSourced},
		"glued text match ignores case":         {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Follow sop-104.", SourceSpanIDs: []string{spanHyphens}}}, []string{}), ""},
		"non-breaking space after sign":         {answer("answered", []ChatStatement{fact("Torque the bolts to -\u00a025 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"superscript digits":                    {answer("answered", []ChatStatement{fact("Torque the bolts to \u00b2\u2075 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"roman numeral":                         {answer("answered", []ChatStatement{fact("Torque the bolts to \u216b Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"line break after sign":                 {answer("answered", []ChatStatement{fact("Torque the bolts to -\n25 Nm.", spanSOP)}, []string{}), CodeNumberNotSourced},
		"spaced source range is not negative":   {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Clearance is -8 mm.", SourceSpanIDs: []string{spanHyphens}}}, []string{}), CodeNumberNotSourced},
		"spaced source range restated":          {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Clearance is 5 to 8 mm.", SourceSpanIDs: []string{spanHyphens}}}, []string{}), ""},
		"spaced sign after a word stays a sign": {answer("answered", []ChatStatement{fact("The offset is -1,250.75 mm.", spanSOP)}, []string{}), ""},
		"narrow no-break space after sign":      {answer("answered", []ChatStatement{fact("Torque the bolts to -\u202f25 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"superscript after digits":              {answer("answered", []ChatStatement{fact("Torque the bolts to 25\u00b2 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"vulgar fraction":                       {answer("answered", []ChatStatement{fact("Torque the bolts to 25\u00bd Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"circled digits":                        {answer("answered", []ChatStatement{fact("Torque the bolts to \u2462 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"em dash in prose is allowed":           {answer("answered", []ChatStatement{fact("Infeed bolts are torqued to 25 Nm \u2014 per the SOP.", spanSOP)}, []string{}), ""},
		"source minus sign is not dropped":      {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Offset is 40 mm.", SourceSpanIDs: []string{spanUnapproved30}}}, []string{}), CodeNumberNotSourced},
		"range key carries the whole number":    {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Station 10 -30 Nm.", SourceSpanIDs: []string{spanHyphens}}}, []string{}), CodeNumberNotSourced},
		"spaced range restated tightly":         {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Clearance is 5-8 mm.", SourceSpanIDs: []string{spanHyphens}}}, []string{}), ""},
		"right-to-left override":                {answer("answered", []ChatStatement{fact("Torque the bolts to \u202e52\u202c Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"directional mark away from numbers":    {answer("answered", []ChatStatement{fact("Infeed bolts\u200f are torqued to 25 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"bidi isolate in missing inputs":        {answer("needs_input", []ChatStatement{}, []string{"Shift plan \u2067x\u2069"}), CodeStatementUnsafe},
		"thin space before a unit is fine":      {answer("answered", []ChatStatement{fact("Infeed bolts are torqued to 25\u2009Nm.", spanSOP)}, []string{}), ""},
		"source no-break space before unit":     {answer("answered", []ChatStatement{fact("The gap is 4 mm.", spanSOP)}, []string{}), ""},
		"thin space joining digit runs":         {answer("answered", []ChatStatement{fact("Infeed bolts are torqued to 25\u2009250 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"no-break space before a digit":         {answer("answered", []ChatStatement{fact("Infeed bolts are torqued to 25\u00a025 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"ogham space mark as a dash":            {answer("answered", []ChatStatement{fact("Torque the bolts to \u168025 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"em space next to a number":             {answer("answered", []ChatStatement{fact("Infeed bolts are torqued to 25\u2003Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"strikethrough mark on a digit":         {answer("answered", []ChatStatement{fact("Infeed bolts are torqued to 2\u03365 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"accent between digit runs":             {answer("answered", []ChatStatement{fact("Infeed bolts are torqued to 25\u0301250 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"keycap on a digit":                     {answer("answered", []ChatStatement{fact("Infeed bolts are torqued to 25\u20e3 Nm.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"combining mark in a word":              {answer("answered", []ChatStatement{fact("Infeed bolts are torqued to 25 Nm per the cafe\u0301 SOP.", spanSOP)}, []string{}), ""},
		"spaced minus is still a sign":          {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Torque to -30 Nm.", SourceSpanIDs: []string{spanHyphens}}}, []string{}), CodeNumberNotSourced},
		// WIS-517: ".45" reads as 0.45; it is not sourced by "45", and "45" is not sourced by ".45".
		".45 is not sourced by 45":  {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Cycle is .45 seconds a part.", SourceSpanIDs: []string{spanCycle45}}}, []string{}), CodeNumberNotSourced},
		"glued about.45 is refused": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Cycle is about.45 s a part.", SourceSpanIDs: []string{spanCycle45}}}, []string{}), CodeStatementUnsafe},
		// WIS-517 review: a label like "Rev.3" is tainted, so it grounds neither 0.x nor the bare integer (fail closed).
		"Rev.3 does not source 0.3": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Dwell is 0.3 s.", SourceSpanIDs: []string{spanRevLabel}}}, []string{}), CodeNumberNotSourced},
		"Rev.3 does not source 3":   {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use Rev 3.", SourceSpanIDs: []string{spanRevLabel}}}, []string{}), CodeNumberNotSourced},
		"v.2 does not source 0.2":   {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Dwell is 0.2 s.", SourceSpanIDs: []string{spanRevLabel}}}, []string{}), CodeNumberNotSourced},
		"No.5 does not source 0.5":  {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Dwell is 0.5 s.", SourceSpanIDs: []string{spanRevLabel}}}, []string{}), CodeNumberNotSourced},
		"1.2.3 does not source 0.3": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Dwell is .3 s.", SourceSpanIDs: []string{spanRevLabel}}}, []string{}), CodeNumberNotSourced},
		"1..7 does not source 0.7":  {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Dwell is 0.7 s.", SourceSpanIDs: []string{spanRevLabel}}}, []string{}), CodeNumberNotSourced},
		// WIS-517 review P2: a range's glued key spells leading-dot components as 0.x too.
		".45-.60 is sourced by 0.45-0.60":  {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Torque .45-.60 Nm.", SourceSpanIDs: []string{spanRange0}}}, []string{}), ""},
		"0.45-0.60 is sourced by .45-.60":  {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Torque 0.45-0.60 Nm.", SourceSpanIDs: []string{spanRangeDot}}}, []string{}), ""},
		"Rev.3-4 does not source rev0.3-4": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Per Rev0.3-4.", SourceSpanIDs: []string{spanRangeDot}}}, []string{}), CodeNumberNotSourced},
		"Rev.3-4 does not source 4":        {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 4.", SourceSpanIDs: []string{spanRangeDot}}}, []string{}), CodeNumberNotSourced},
		// WIS-517 review: an answer range with a tainted component is refused, not skipped.
		"answer x:.3-99 refused": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Dwell x:.3-99 s.", SourceSpanIDs: []string{spanRange0}}}, []string{}), CodeStatementUnsafe},
		"answer Rev.3-4 refused": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Per Rev.3-4.", SourceSpanIDs: []string{spanRangeDot}}}, []string{}), CodeStatementUnsafe},
		"answer a=.5-.9 refused": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Set a=.5-.9 Nm.", SourceSpanIDs: []string{spanRange0}}}, []string{}), CodeStatementUnsafe},
		"answer ab.c-99 refused": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Set ab.c-99 Nm.", SourceSpanIDs: []string{spanRange0}}}, []string{}), CodeStatementUnsafe},
		// WIS-517 review: spaces around a range's dash don't change the 0.x spelling of its parts.
		"spaced .45 - .60 is sourced by 0.45-0.60": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Torque .45 - .60 Nm.", SourceSpanIDs: []string{spanRange0}}}, []string{}), ""},
		"0.45-0.60 is sourced by spaced .45 - .60": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Torque 0.45-0.60 Nm.", SourceSpanIDs: []string{spanRangeSpaced}}}, []string{}), ""},
		"answer SOP.A-999 refused":                 {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Follow SOP.A-999.", SourceSpanIDs: []string{spanRange0}}}, []string{}), CodeStatementUnsafe},
		// WIS-517 review: exponent notation is tainted whole, never read as its mantissa or exponent.
		".45e2 does not source 0.45":                 {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Cycle 0.45 seconds.", SourceSpanIDs: []string{spanExponent}}}, []string{}), CodeNumberNotSourced},
		".45e2 does not source 2":                    {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Cycle 2 seconds.", SourceSpanIDs: []string{spanExponent}}}, []string{}), CodeNumberNotSourced},
		".45e2 does not source .3 either":            {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Cycle .3 seconds.", SourceSpanIDs: []string{spanExponent}}}, []string{}), CodeNumberNotSourced},
		"answer 4.5e1 refused":                       {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Cycle 4.5e1 seconds.", SourceSpanIDs: []string{spanExponent}}}, []string{}), CodeStatementUnsafe},
		"answer 4.5E+1 refused":                      {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Cycle 4.5E+1 seconds.", SourceSpanIDs: []string{spanExponent}}}, []string{}), CodeStatementUnsafe},
		"dot-mantissa exponent does not ground 4":    {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 4.", SourceSpanIDs: []string{spanExponent}}}, []string{}), CodeNumberNotSourced},
		"dot-mantissa exponent does not ground 1":    {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 1.", SourceSpanIDs: []string{spanExponent}}}, []string{}), CodeNumberNotSourced},
		"dot-mantissa exponent does not ground -1":   {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use -1.", SourceSpanIDs: []string{spanExponent}}}, []string{}), CodeNumberNotSourced},
		"dot-mantissa exponent does not ground 0.45": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 0.45.", SourceSpanIDs: []string{spanExponent}}}, []string{}), CodeNumberNotSourced},
		"answer 45.e2 refused":                       {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 45.e2.", SourceSpanIDs: []string{spanExponent}}}, []string{}), CodeStatementUnsafe},
		"answer 4.E+1 refused":                       {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 4.E+1.", SourceSpanIDs: []string{spanExponent}}}, []string{}), CodeStatementUnsafe},
		"answer 45.E-1 refused":                      {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 45.E-1.", SourceSpanIDs: []string{spanExponent}}}, []string{}), CodeStatementUnsafe},
		"control: 45 each grounds 45":                {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Pack 45 each.", SourceSpanIDs: []string{spanExponent}}}, []string{}), ""},
		"control: 3e grounds 3":                      {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Grade 3e.", SourceSpanIDs: []string{spanExponent}}}, []string{}), ""},
		// WIS-517 review round 6: prose punctuation after a label ends the exponent gap.
		"label 3E, 25 restated verbatim": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Rack 3E, 25 parts.", SourceSpanIDs: []string{spanLabelExp}}}, []string{}), ""},
		"label 3E, grounds 25":           {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "25 parts.", SourceSpanIDs: []string{spanLabelExp}}}, []string{}), ""},
		"label 3e. grounds 40":           {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "40 trays.", SourceSpanIDs: []string{spanLabelExp}}}, []string{}), ""},
		"label 5E = grounds 30":          {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Shelf holds 30 bins.", SourceSpanIDs: []string{spanLabelExp}}}, []string{}), ""},
		// WIS-517 review round 7: every invisible rune counts in the exponent gap (U+FE0F, U+034F).
		"variation-selector gap does not ground 0.65": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Dwell 0.65 s.", SourceSpanIDs: []string{spanFillerGap}}}, []string{}), CodeNumberNotSourced},
		"grapheme-joiner gap does not ground 0.75":    {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Dwell 0.75 s.", SourceSpanIDs: []string{spanFillerGap}}}, []string{}), CodeNumberNotSourced},
		// WIS-517 review round 8: a U+FB29 sign, a non-ASCII exponent digit, an invisible rune before e.
		"fb29 sign does not ground 0.35":                 {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Hold 0.35 s.", SourceSpanIDs: []string{spanFillerGap}}}, []string{}), CodeNumberNotSourced},
		"full-width exponent digit does not ground 0.55": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Hold 0.55 s.", SourceSpanIDs: []string{spanFillerGap}}}, []string{}), CodeNumberNotSourced},
		"superscript exponent does not ground 0.85":      {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Hold 0.85 s.", SourceSpanIDs: []string{spanFillerGap}}}, []string{}), CodeNumberNotSourced},
		"invisible before e does not ground 7":           {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Hold 7 s.", SourceSpanIDs: []string{spanFillerGap}}}, []string{}), CodeNumberNotSourced},
		"invisible before e does not ground 0.95":        {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Hold 0.95 s.", SourceSpanIDs: []string{spanFillerGap}}}, []string{}), CodeNumberNotSourced},
		"filler before e does not ground 0.15":           {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Hold 0.15 s.", SourceSpanIDs: []string{spanFillerGap}}}, []string{}), CodeNumberNotSourced},
		"filler before e does not ground 9":              {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Hold 9 s.", SourceSpanIDs: []string{spanFillerGap}}}, []string{}), CodeNumberNotSourced},
		"label 7e: grounds 12":                           {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Bin holds 12 kg.", SourceSpanIDs: []string{spanLabelExp}}}, []string{}), ""},
		// WIS-517 review: an exponent's sign may be any non-letter run; only an ASCII letter makes an identifier.
		"answer .45e\u207b2 refused":                      {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use .45e\u207b2.", SourceSpanIDs: []string{spanFillerExp}}}, []string{}), CodeStatementUnsafe},
		"answer .45e 2 refused":                           {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use .45e 2.", SourceSpanIDs: []string{spanFillerExp}}}, []string{}), CodeStatementUnsafe},
		"answer .45e\uff0d2 refused":                      {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use .45e\uff0d2.", SourceSpanIDs: []string{spanFillerExp}}}, []string{}), CodeStatementUnsafe},
		"answer .45e\u20142 refused":                      {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use .45e\u20142.", SourceSpanIDs: []string{spanFillerExp}}}, []string{}), CodeStatementUnsafe},
		"answer \u316445e2 refused":                       {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use \u316445e2.", SourceSpanIDs: []string{spanFillerExp}}}, []string{}), CodeStatementUnsafe},
		"answer \uffa045e2 refused":                       {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use \uffa045e2.", SourceSpanIDs: []string{spanFillerExp}}}, []string{}), CodeStatementUnsafe},
		"wide exponent source does not ground 0.45":       {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 0.45.", SourceSpanIDs: []string{spanFillerExp}}}, []string{}), CodeNumberNotSourced},
		"wide exponent source does not ground 45":         {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 45.", SourceSpanIDs: []string{spanFillerExp}}}, []string{}), CodeNumberNotSourced},
		"wide exponent source does not ground 2":          {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 2.", SourceSpanIDs: []string{spanFillerExp}}}, []string{}), CodeNumberNotSourced},
		"filler-gap exponent source does not ground 0.45": {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 0.45.", SourceSpanIDs: []string{spanFillerGap}}}, []string{}), CodeNumberNotSourced},
		"filler-gap exponent source does not ground 2":    {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 2.", SourceSpanIDs: []string{spanFillerGap}}}, []string{}), CodeNumberNotSourced},
		"filler-gap exponent source does not ground 55":   {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 55.", SourceSpanIDs: []string{spanFillerGap}}}, []string{}), CodeNumberNotSourced},
		"answer with filler .45e\u3164-2 refused":         {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use .45e\u3164-2.", SourceSpanIDs: []string{spanFillerGap}}}, []string{}), CodeStatementUnsafe},
		"answer with filler Cycle\u3164 ok refused":       {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use Cycle\u3164 ok.", SourceSpanIDs: []string{spanFillerGap}}}, []string{}), CodeStatementUnsafe},
		"M4E16.45e2 does not ground 0.45":                 {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 0.45.", SourceSpanIDs: []string{spanIdentExp}}}, []string{}), CodeNumberNotSourced},
		"M4E16.45e2 does not ground 45":                   {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 45.", SourceSpanIDs: []string{spanIdentExp}}}, []string{}), CodeNumberNotSourced},
		"M4E16.45e2 does not ground 2":                    {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 2.", SourceSpanIDs: []string{spanIdentExp}}}, []string{}), CodeNumberNotSourced},
		"M4E16.45e2 does not ground 16.45":                {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 16.45.", SourceSpanIDs: []string{spanIdentExp}}}, []string{}), CodeNumberNotSourced},
		"M4E16e2 does not ground 16":                      {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 16.", SourceSpanIDs: []string{spanIdentExp}}}, []string{}), CodeNumberNotSourced},
		"M4E16e2 does not ground 2":                       {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 2.", SourceSpanIDs: []string{spanIdentExp}}}, []string{}), CodeNumberNotSourced},
		"M4E16e2 does not ground 4":                       {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 4.", SourceSpanIDs: []string{spanIdentExp}}}, []string{}), CodeNumberNotSourced},
		"answer M4E16e2 refused":                          {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Then M4E16e2.", SourceSpanIDs: []string{spanIdentExp}}}, []string{}), CodeStatementUnsafe},
		"answer M4E16.45e2 refused":                       {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use M4E16.45e2 now.", SourceSpanIDs: []string{spanIdentExp}}}, []string{}), CodeStatementUnsafe},
		// WIS-517 review: a number glued to a tainted one by - / : x is tainted too.
		"P1 source does not ground 0.45":                    {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 0.45.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeNumberNotSourced},
		"P1 source does not ground 0.55":                    {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 0.55.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeNumberNotSourced},
		"P1 source does not ground 6":                       {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 6.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeNumberNotSourced},
		"P1 source does not ground 8":                       {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 8.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeNumberNotSourced},
		"P1 source does not ground 9":                       {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 9.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeNumberNotSourced},
		"P1 source does not ground 999":                     {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 999.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeNumberNotSourced},
		"answer .45e\u22122 refused":                        {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use .45e\u22122.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeStatementUnsafe},
		"answer .55e+ 3 refused":                            {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use .55e+ 3.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeStatementUnsafe},
		"answer SOP.A-999/6 refused":                        {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use SOP.A-999/6.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeStatementUnsafe},
		"identifier M4E16 cited verbatim is accepted":       {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use M4E16.", SourceSpanIDs: []string{spanNote}}}, []string{}), ""},
		"glued-taint source does not ground 7":              {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 7.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeNumberNotSourced},
		"glued-taint source does not ground -7":             {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use -7.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeNumberNotSourced},
		"glued-taint source does not ground 5":              {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 5.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeNumberNotSourced},
		"glued-taint source does not ground 1":              {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 1.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeNumberNotSourced},
		"glued-taint source does not ground 4":              {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 4.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeNumberNotSourced},
		"glued-taint source does not ground 0.3":            {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 0.3.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeNumberNotSourced},
		"glued-taint source does not ground 3":              {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 3.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeNumberNotSourced},
		"glued-taint source does not ground 2":              {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 2.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeNumberNotSourced},
		"glued-taint source does not ground 0.2":            {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 0.2.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeNumberNotSourced},
		"answer 1e5-7 refused":                              {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use 1e5-7.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeStatementUnsafe},
		"answer Rev.3/4 refused":                            {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use Rev.3/4.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeStatementUnsafe},
		"answer v.2:5 refused":                              {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Use v.2:5.", SourceSpanIDs: []string{spanGluedTaint}}}, []string{}), CodeStatementUnsafe},
		"answer x:.3 refused":                               {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Dwell x:.3 s.", SourceSpanIDs: []string{spanCycle045}}}, []string{}), CodeStatementUnsafe},
		"a=.3 does not source 0.3":                          {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Dwell is 0.3 s.", SourceSpanIDs: []string{spanPunct}}}, []string{}), CodeNumberNotSourced},
		"answer label Rev.3 refused":                        {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Per Rev.3 apply.", SourceSpanIDs: []string{spanRevLabel}}}, []string{}), CodeStatementUnsafe},
		"45 is not sourced by .45":                          {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Cycle is 45 seconds a part.", SourceSpanIDs: []string{spanCycleDot45}}}, []string{}), CodeNumberNotSourced},
		".45 is sourced by .45":                             {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Cycle is .45 seconds a part.", SourceSpanIDs: []string{spanCycleDot45}}}, []string{}), ""},
		".45 is sourced by 0.45":                            {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Cycle is .45 seconds a part.", SourceSpanIDs: []string{spanCycle045}}}, []string{}), ""},
		"0.45 is sourced by .45":                            {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Cycle is 0.45 seconds a part.", SourceSpanIDs: []string{spanCycleDot45}}}, []string{}), ""},
		"number elsewhere outside cited span":               {answer("answered", []ChatStatement{{Kind: "reported_observation", Text: "Retorque every 30 min.", SourceSpanIDs: []string{spanNote}}}, []string{}), CodeNumberNotSourced},
		"approved citation cannot label unapproved number":  {answer("answered", []ChatStatement{fact("Retorque every 30 min.", spanApprovedPlain, spanUnapproved30)}, []string{}), CodeFactNotApproved},
		"approved citation cannot label unapproved wording": {answer("answered", []ChatStatement{fact("Bolts must be clean and inspected each shift.", spanApprovedPlain, spanNote)}, []string{}), CodeFactNotApproved},
		"model cannot impersonate stale snapshot label":     {answer("answered", []ChatStatement{fact("Based \n on the APPROVED process snapshot of 2026-10-28T14:00:00Z", spanSOP)}, []string{}), CodeStatementUnsafe},
		"approved fact without an approved claim span":      {answer("answered", []ChatStatement{fact("Station 3 is inspected each shift.", spanNote)}, []string{}), CodeFactNotApproved},
		"citation of a span never retrieved":                {answer("answered", []ChatStatement{fact("Bolts are torqued.", spanUnread)}, []string{}), CodeCitationNotInScope},
		"duplicate citation":                                {answer("answered", []ChatStatement{fact("Bolts are torqued.", spanSOP, spanSOP)}, []string{}), CodeCitationNotInScope},
		"statement without citations":                       {answer("answered", []ChatStatement{fact("Bolts are torqued.")}, []string{}), CodeCitationNotInScope},
		"link in a statement":                               {answer("answered", []ChatStatement{fact("See https://evil.example for torque.", spanSOP)}, []string{}), CodeStatementUnsafe},
		"credential request in missing inputs":              {answer("needs_input", []ChatStatement{}, []string{"your password"}), CodeStatementUnsafe},
		"answered without statements":                       {answer("answered", []ChatStatement{}, []string{}), CodeAnswerInvalid},
		"needs input with statements":                       {answer("needs_input", []ChatStatement{fact("Bolts are torqued.", spanSOP)}, []string{"x"}), CodeAnswerInvalid},
		"unknown status":                                    {answer("approved", []ChatStatement{fact("Bolts are torqued.", spanSOP)}, []string{}), CodeAnswerInvalid},
		"unknown kind":                                      {answer("answered", []ChatStatement{{Kind: "instruction", Text: "Restart line two.", SourceSpanIDs: []string{spanSOP}}}, []string{}), CodeAnswerInvalid},
		"wrong run id":                                      {ChatAnswerOutputV2{SchemaVersion: ChatSchemaVersion, RunID: spanSOP, Role: RoleDiscoveryMapping, Status: "answered", Statements: []ChatStatement{fact("Bolts are torqued.", spanSOP)}, MissingInputs: []string{}}, CodeAnswerInvalid},
		"too many statements":                               {answer("answered", []ChatStatement{fact("a", spanSOP), fact("b", spanSOP), fact("c", spanSOP), fact("d", spanSOP), fact("e", spanSOP), fact("f", spanSOP), fact("g", spanSOP)}, []string{}), CodeAnswerInvalid},
		"statement over 1000 runes":                         {answer("answered", []ChatStatement{fact(strings.Repeat("é", 1001), spanSOP)}, []string{}), CodeAnswerInvalid},
	} {
		if got := validateChatOutput(c.out, child, spanSource, sources, approved); got != c.want {
			t.Errorf("%s: code %q, want %q", name, got, c.want)
		}
	}
}

// WIS-517: a decimal point with no digit before it stays part of the number.
func TestChatNumbersLeadingDecimalPoint(t *testing.T) {
	for in, want := range map[string][]string{
		".45 seconds":                           {"0.45"},
		"about.45 s":                            nil,
		"Rev.3 v.2 No.5":                        nil,
		"x:.3 a=.3 #.3 /.3 b;.3 c,.3 d_.3 e*.3": nil,
		" .45 \u201c.5\u201d \u00b1.25":         {"0.45", "0.5", "0.25"},
		"(.45) \".5\"":                          {"0.45", "0.5"},
		"-.45 mm":                               {"-0.45"},
		"1.45 and 1,234":                        {"1.45", "1,234"},
		"0.45":                                  {"0.45"},
		"torque 20-30 Nm":                       {"20", "30"},
		"offset -12 mm":                         {"-12"},
	} {
		var got []string
		for _, tok := range chatNumbers(in) {
			got = append(got, tok.n)
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("chatNumbers(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSourceSpanTextsUsesRuneOffsets(t *testing.T) {
	got, valid := sourceSpanTexts("甲5 25乙", []sourceSpan{{id: spanSOP, start: 3, end: 5}})
	if !valid || got[spanSOP] != "25" {
		t.Fatalf("span text %q valid=%v, want %q", got[spanSOP], valid, "25")
	}
	if _, valid = sourceSpanTexts("甲5", []sourceSpan{{id: spanSOP, start: 1, end: 3}}); valid {
		t.Fatal("out-of-bounds span was accepted")
	}
}

// WIS-517 M2: approval is derived from the pinned snapshot. A claim approves its spans only when
// every one of them lies in a captured source revision; resolution peers approve nothing.
func TestApprovedChatSpans(t *testing.T) {
	a, b := capturedSource{"source-a", 1}, capturedSource{"source-b", 1}
	contents := knowledge.SnapshotContents{
		Spans: []knowledge.SnapshotSpan{{SpanID: "a1", SourceID: a.id, Revision: 1}, {SpanID: "a2", SourceID: a.id, Revision: 1},
			{SpanID: "b1", SourceID: b.id, Revision: 1}, {SpanID: "peer", SourceID: a.id, Revision: 1}},
		Claims: []knowledge.SnapshotClaim{
			{ClaimInput: knowledge.ClaimInput{SourceSpanIDs: []string{"a1"}}},
			{ClaimInput: knowledge.ClaimInput{SourceSpanIDs: []string{"a2", "b1"}}},
			{ClaimInput: knowledge.ClaimInput{SourceSpanIDs: []string{"unpinned"}}},
		},
	}
	for _, tc := range []struct {
		name     string
		captured map[capturedSource]bool
		want     []string
	}{
		{"all sources captured", map[capturedSource]bool{a: true, b: true}, []string{"a1", "a2", "b1"}},
		{"one source of a claim not captured", map[capturedSource]bool{a: true}, []string{"a1"}},
		{"captured at another revision", map[capturedSource]bool{{"source-a", 2}: true, b: true}, nil},
		{"nothing captured", nil, nil},
	} {
		got := approvedChatSpans(contents, tc.captured)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
		for _, id := range tc.want {
			if !got[id] {
				t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
			}
		}
	}
}

// The reply body is rebuilt from validated statements with numbered citations; nothing else of
// the model output reaches the user.
func TestRenderChatAnswer(t *testing.T) {
	spanSource := map[string]string{spanSOP: chatSource, spanNote: chatSource}
	sources := map[string]sourceRead{chatSource: {revision: 2}}
	approved := map[string]bool{spanSOP: true}
	body, citations := renderChatAnswer(ChatAnswerOutputV2{Status: "answered", Statements: []ChatStatement{
		{Kind: "approved_fact", Text: " Bolts are torqued. ", SourceSpanIDs: []string{spanSOP}},
		{Kind: "hypothesis", Text: "Vibration may loosen them.", SourceSpanIDs: []string{spanNote, spanSOP}},
	}}, spanSource, sources, approved, nil)
	if want := "- [Approved process] Bolts are torqued. [1]\n- [Hypothesis] Vibration may loosen them. [2] [1]"; body != want {
		t.Fatalf("body:\n%s\nwant:\n%s", body, want)
	}
	if len(citations) != 2 || citations[0].ID != spanSOP || citations[1].ID != spanNote || citations[0].Revision != 2 || citations[0].Kind != "evidence_span" {
		t.Fatalf("citations: %+v", citations)
	}
	stale := ChatSnapshotProvenance{SnapshotCreatedAt: time.Date(2026, 10, 28, 14, 0, 0, 0, time.FixedZone("UTC-4", -4*60*60)), SnapshotStale: true}
	body, _ = renderChatAnswer(ChatAnswerOutputV2{Status: "answered", Statements: []ChatStatement{{Kind: "approved_fact", Text: "Bolts are torqued.", SourceSpanIDs: []string{spanSOP}}}}, spanSource, sources, approved, &stale)
	if want := "- [Approved process] Bolts are torqued. [1]\n\nBased on the approved process snapshot of 2026-10-28T18:00:00Z; newer knowledge edits are not yet approved."; body != want {
		t.Fatalf("stale snapshot label %q, want %q", body, want)
	}
	body, citations = renderChatAnswer(ChatAnswerOutputV2{Status: "needs_input", Statements: []ChatStatement{}, MissingInputs: []string{"Shift plan for line 2"}}, spanSource, sources, approved, nil)
	if !strings.Contains(body, "Missing input:\n- Shift plan for line 2") || len(citations) != 0 {
		t.Fatalf("needs input body %q citations %+v", body, citations)
	}
}

// WIS-517 live defect: a statement resting only on approved spans was shown as "Reported
// observation" because the label was the model's choice. The server settles it from the pinned
// snapshot; a hypothesis is never promoted, and an unapproved citation keeps the observation label.
func TestRenderChatAnswerSettlesApprovedLabel(t *testing.T) {
	spanSource := map[string]string{spanSOP: chatSource, spanApprovedPlain: chatSource, spanNote: chatSource}
	sources := map[string]sourceRead{chatSource: {revision: 2}}
	approved := map[string]bool{spanSOP: true, spanApprovedPlain: true}
	for name, c := range map[string]struct {
		s    ChatStatement
		want string
	}{
		"observation on approved spans is approved": {ChatStatement{Kind: "reported_observation", Text: "Count only accepted parts.", SourceSpanIDs: []string{spanSOP, spanApprovedPlain}}, "- [Approved process] Count only accepted parts. [1] [2]"},
		"observation with an unapproved span stays": {ChatStatement{Kind: "reported_observation", Text: "Count only accepted parts.", SourceSpanIDs: []string{spanSOP, spanNote}}, "- [Reported observation] Count only accepted parts. [1] [2]"},
		"hypothesis on approved spans stays":        {ChatStatement{Kind: "hypothesis", Text: "Rework may delay the count.", SourceSpanIDs: []string{spanSOP}}, "- [Hypothesis] Rework may delay the count. [1]"},
	} {
		if body, _ := renderChatAnswer(ChatAnswerOutputV2{Status: "answered", Statements: []ChatStatement{c.s}}, spanSource, sources, approved, nil); body != c.want {
			t.Errorf("%s: body %q, want %q", name, body, c.want)
		}
	}
}
