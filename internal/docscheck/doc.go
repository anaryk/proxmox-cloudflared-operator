// Package docscheck holds the tests that keep the reference pages of docs/ in
// step with the code: every setting, every environment variable, every event
// kind, doctor check, named problem line, route state, held reason, waiting
// kind and writer verdict has its row, and so has every route of the socket
// and every call the web interface's gateway passes on, whose admin calls
// each name their action on the page of the web interface. The package has
// no code of its own; a change that adds one of them fails here until its
// page says what it is.
package docscheck
