import type { Library, PluginAdminFormField } from "@/api/types";

import type { SchemaOption } from "./schemaFormUtils";

/**
 * A library picker's scope, carried on an inferred admin form field. The value
 * is one of the host-known JSON Schema `format` values on a plugin config
 * property.
 */
export type LibraryPickerKind = NonNullable<PluginAdminFormField["library_picker"]>;

/**
 * Map a JSON Schema `format` to a library picker kind. Unknown formats return
 * null so property inference keeps its url/password/boolean/number behaviour.
 */
export function libraryPickerForFormat(format: string | undefined): LibraryPickerKind | null {
  switch (format) {
    case "silo-library":
      return "any";
    case "silo-library-movie":
      return "movie";
    case "silo-library-tv":
      return "tv";
    default:
      return null;
  }
}

/**
 * Normalise a library's declared type to a movie/TV/mixed kind.
 *
 * Mirrors the private `libraryKind` in
 * src/pages/admin/autoscan/sourceDescriptor.ts: that helper predates this module
 * and the autoscan lane owns it, so it is copied here with this note rather than
 * shared across a page/component boundary. Keep the two in sync.
 */
export function libraryKind(type: string): "movie" | "tv" | "mixed" | null {
  switch (type.trim().toLowerCase()) {
    case "movie":
    case "movies":
      return "movie";
    case "series":
    case "show":
    case "shows":
    case "tv":
    case "tvshows":
      return "tv";
    case "mixed":
      return "mixed";
    default:
      return null;
  }
}

/**
 * Whether an enabled library belongs in a picker. A `mixed` library holds both
 * movies and TV, so it satisfies either restricted picker; `any` accepts every
 * enabled library. Disabled libraries are never offered.
 */
export function libraryMatchesPicker(kind: LibraryPickerKind, library: Library): boolean {
  if (!library.enabled) return false;
  if (kind === "any") return true;
  const normalized = libraryKind(library.type);
  return normalized === kind || normalized === "mixed";
}

/**
 * The options a library picker offers, labelled `Name (ID)` and valued by the
 * library's numeric id as a string — the value the plugin receives.
 */
export function libraryPickerOptions(
  kind: LibraryPickerKind,
  libraries: readonly Library[],
): SchemaOption[] {
  return libraries
    .filter((library) => libraryMatchesPicker(kind, library))
    .map((library) => ({ value: String(library.id), label: `${library.name} (${library.id})` }));
}
