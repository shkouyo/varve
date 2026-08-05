"use strict";

/**
 * html-validate transformer for raw Go html/template files.
 *
 * The templates are validated in their raw form (CI runs html-validate over
 * the internal/web/templates directory), so every Go template action must be
 * removed before tokenization: actions inlined in attribute values (e.g.
 * class="... {{ if eq .Nav "dashboard" }}...") contain double quotes that
 * would otherwise close the attribute value early and make the tokenizer fail
 * with parser-error/attr-spacing.
 *
 * Each action is replaced with a unique placeholder `xN` (N increments per
 * action) padded with spaces when it directly abuts non-whitespace, and
 * newlines inside the action are preserved so reported line numbers still
 * point at the original template lines. This keeps the document valid:
 *   - actions inside attribute values leave a legal, non-empty value;
 *   - actions between attributes or tags leave whitespace-separated
 *     placeholders, so checks like wcag/h30 (anchor text), empty-heading and
 *     attribute-allowed-values see present content instead of nothing;
 *   - a unique placeholder per action avoids duplicated-attribute reports.
 *
 * API: https://html-validate.org/dev/transformers.html
 * (Transformer function, api version 1, default export).
 */

/**
 * Replace every Go template action in `source` with placeholder text.
 *
 * Handles:
 *   - plain actions:            {{ .Nav }}
 *   - control flow:             {{ if eq .Nav "dashboard" }}...{{ end }}
 *   - trim markers:             {{- ... -}}
 *   - comments:                 template comments (start delimiter, body, end delimiters)
 *   - quoted strings in action: closing delimiter inside a quoted string
 *   - actions spanning lines    (newlines preserved)
 *
 * @param {string} source
 * @returns {string}
 */
function stripGoActions(source) {
  let out = "";
  let i = 0;
  const n = source.length;
  let actionCount = 0;
  let inValue = false; // inside a quoted attribute value

  while (i < n) {
    const open = source.indexOf("{{", i);
    if (open === -1) {
      out += source.slice(i);
      break;
    }
    out += source.slice(i, open);

    // Track quoted attribute values through the text between actions.
    for (let k = i; k < open; k++) {
      const ch = source[k];
      if (inValue) {
        if (ch === '"' || ch === "'") {
          inValue = false;
        }
      } else if (ch === '"' || ch === "'") {
        if (isValueQuoteStart(source, k)) {
          inValue = true;
        }
      }
    }

    let j = open + 2;
    if (source.startsWith("/*", j)) {
      // Template comment: runs to the first comment close, then the delimiter.
      const close = source.indexOf("*/", j + 2);
      j = close === -1 ? n : close + 2;
      if (source.startsWith("}}", j)) {
        j += 2;
      }
    } else {
      // Action: runs to the first }} that is not inside a quoted string.
      let inString = false;
      while (j < n) {
        const ch = source[j];
        if (inString) {
          if (ch === "\\") {
            j += 2;
            continue;
          }
          if (ch === '"' || ch === "`") {
            inString = false;
          }
        } else if (ch === '"' || ch === "`") {
          inString = true;
        } else if (source.startsWith("}}", j)) {
          j += 2;
          break;
        }
        j++;
      }
    }

    // Build the placeholder: unique per action, newlines inside the action
    // preserved. Pad with spaces only where the action directly abuts
    // non-whitespace *outside* a quoted attribute value (padding inside a
    // value would corrupt URLs etc.); between attributes and in text content
    // the padding keeps attributes separated so no attr-spacing/dup-attr
    // reports are produced.
    const before = open > 0 ? source[open - 1] : "";
    const after = j < n ? source[j] : "";
    const padBefore = before !== "" && !/\s/.test(before) && !inValue;
    const padAfter = after !== "" && !/\s/.test(after) && !inValue;
    const placeholder = "x" + actionCount++;

    let replacement = (padBefore ? " " : "") + placeholder + (padAfter ? " " : "");
    const end = Math.min(j, n);
    for (let k = open; k < end; k++) {
      if (source[k] === "\n") {
        replacement += "\n";
      }
    }
    out += replacement;
    i = end;
  }

  return out;
}

/**
 * True if the quote at `index` opens an attribute value, i.e. the previous
 * non-whitespace character is `=` (e.g. the quote in `class="`).
 *
 * @param {string} source
 * @param {number} index
 * @returns {boolean}
 */
function isValueQuoteStart(source, index) {
  for (let k = index - 1; k >= 0; k--) {
    const ch = source[k];
    if (/\s/.test(ch)) {
      continue;
    }
    return ch === "=";
  }
  return false;
}

/**
 * @param {import("html-validate").Source} source
 * @returns {import("html-validate").Source}
 */
function goTemplates(source) {
  return { ...source, data: stripGoActions(source.data) };
}

goTemplates.api = 1;

module.exports = goTemplates;
