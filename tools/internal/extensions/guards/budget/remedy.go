package budget

// remedy.go — the two gates' breach guidance, and the reason it is a value rather
// than a constant in the engine.
//
// A budget gate's OUTPUT IS ITS PRODUCT: the number only teaches if the message
// beside it says what to do, and the two gates that share this engine want
// OPPOSITE things done. untestable-loc says "move the logic into unit-tested Go";
// core-surface says "move it out of the wiring layer" (ADR 0014). One message
// saying both would be a riddle, so the remedy travels with the gate and the
// engine only substitutes `{config}` into it.
//
// THEY BOTH NAME tools/internal NOW, AND THAT IS NOT THE COLLISION IT LOOKS LIKE.
// Both remedies used to be phrased against package main — untestable-loc pointing
// IN, core-surface pointing OUT — and that opposition was the whole reason they
// are two constants. The move emptied tools/cmd/llz to six lines, which retired
// the destination untestable-loc was naming without retiring the gate: it went on
// telling operators to move logic into a package the OTHER gate caps at six lines
// exactly, so following one message failed the other. The axes are still
// opposite; they are just no longer opposite about the same directory.
// untestable-loc is about the LANGUAGE (bash → Go, anywhere testable);
// core-surface is about the LAYER (out of internal/cli, into a package that owns
// the concern). A file can satisfy both, and before this it could not.
//
// `{config}` is a placeholder rather than a %s verb because the alternative remedy
// comes from YAML, where a stray % would turn into a format error.

// UntestableRemedy is `llz ci untestable-loc`'s guidance. Its doctrine is
// absolute — the budget never goes up — because the logic it counts has somewhere
// unambiguously better to be.
const UntestableRemedy = "Move the logic into unit-tested Go " +
	"(tools/internal/<pkg> — NOT tools/cmd/llz, which core-surface caps at six lines), " +
	"or — for genuine install/glue with no logic — add the file to " +
	"`exclude:` in {config} with a justification. Do NOT raise the budget to make this pass."

// CoreSurfaceRemedy is `llz ci core-surface`'s guidance, and the ONLY copy of it:
// .core-surface-budget.yaml deliberately sets no `remedy:` key, because when the
// wording lived in both places the YAML copy silently won and edits here never
// reached an operator.
//
// It has two branches because the number is a high-water mark with no slack, so a
// breach is routine rather than exceptional. Preferred: decompose, and the number
// goes DOWN. Otherwise: record the growth on the same line in the same commit,
// where a reviewer sees it next to the code that caused it. Telling authors never
// to raise it — untestable-loc's doctrine — would be wrong here, and would get the
// gate deleted rather than obeyed.
//
// IT NO LONGER SAYS "package main", because the tree is not there any more. The
// budget's subject is tools/internal/cli — the wiring layer — and the remedy has to
// name what a reader should actually do, which is get logic out of the layer whose
// only job is construction. Saying "package main" would send them to a six-line
// stub and read as a gate nobody maintains.
const CoreSurfaceRemedy = "The CLI wiring layer grew (ADR 0014). Prefer to shrink it: extract to " +
	"tools/internal/<pkg> (ADR 0013), move the capability out to an extension (issue #10), " +
	"or delete what is dead. If the growth is intended, update the number in {config} in THIS " +
	"commit and say in the message why the code belongs in the layer that only wires commands up."
