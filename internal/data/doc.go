// Package data makes copies of a database that are safe to share with CI:
// every column of every table has a rule (classify.yaml), personal values
// are replaced deterministically under a project key, and the copy goes to
// another database and/or a byte-identical SQL dump. It is pseudonymisation,
// not anonymisation in the sense of the GDPR: whoever holds the key and the
// original data can link the two.
//
// The package stands alone (no command-line or configuration code), so it
// can be published separately later. Entry points:
//
//   - ReadSchema: tables, columns, primary and foreign keys (Schema.Order,
//     Schema.References for a row graph);
//   - Classify and MergeRules: proposals for columns without a rule;
//   - LoadRules, ParseRules, Rules.Check: the rules and their fit to a schema;
//   - Anonymize: database → database and/or dump, streaming;
//   - Subset: as Options.Filter, a sample with closed references (the
//     subset: section of classify.yaml);
//   - PersonalValues: the original personal values, for a leak check;
//   - Verify: the two checks of an output (detector and leak check);
//   - NewReplacer: the strategies on single values.
//
// MySQL, MariaDB and PostgreSQL are supported.
package data
