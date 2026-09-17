import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Library, PluginAdminForm } from "@/api/types";
import { SchemaForm } from "./SchemaForm";

// jsdom lacks the pointer-capture API Radix Select calls when opening.
window.HTMLElement.prototype.hasPointerCapture ??= () => false;
window.HTMLElement.prototype.scrollIntoView ??= () => {};

const { librariesState, useAdminLibrariesCalls } = vi.hoisted(() => ({
  librariesState: { current: [] as Library[] },
  useAdminLibrariesCalls: [] as Array<{ enabled?: boolean } | undefined>,
}));
vi.mock("@/hooks/queries/admin/libraries", () => ({
  useAdminLibraries: (options?: { enabled?: boolean }) => {
    useAdminLibrariesCalls.push(options);
    return { data: librariesState.current };
  },
}));

function library(overrides: Partial<Library> & Pick<Library, "id" | "name" | "type">): Library {
  return {
    paths: [],
    enabled: true,
    metadata_language: "",
    auto_translate_metadata: false,
    chapter_thumbnails_enabled: false,
    chapter_thumbnails_supported: false,
    intro_detection_enabled: false,
    trailer_kinds: [],
    sort_order: 0,
    last_scanned_at: null,
    ...overrides,
  };
}

const LIBRARIES: Library[] = [
  library({ id: 1, name: "Movies", type: "movie" }),
  library({ id: 3, name: "Shows", type: "series" }),
  library({ id: 5, name: "Mixed", type: "mixed" }),
  library({ id: 6, name: "Disabled movies", type: "movie", enabled: false }),
];

function libraryPickerDescriptor(picker: "any" | "movie" | "tv"): PluginAdminForm {
  return {
    fields: [
      {
        key: "library_id",
        label: "Library",
        control: "SELECT",
        required: false,
        secret: false,
        multiline: false,
        library_picker: picker,
      },
    ],
  };
}

beforeEach(() => {
  librariesState.current = [];
  useAdminLibrariesCalls.length = 0;
});

const descriptor: PluginAdminForm = {
  fields: [
    {
      key: "service_kind",
      label: "Service",
      control: "SELECT",
      required: true,
      secret: false,
      multiline: false,
      options: [
        { value: "radarr", label: "Radarr" },
        { value: "sonarr", label: "Sonarr" },
      ],
    },
    {
      key: "season_folder",
      label: "Season folder",
      control: "SWITCH",
      required: false,
      secret: false,
      multiline: false,
      show_when: [{ field: "service_kind", equals: ["sonarr"] }],
    },
    {
      key: "root_folder",
      label: "Root folder",
      control: "SELECT",
      required: false,
      secret: false,
      multiline: false,
      dynamic_options: true,
    },
  ],
  sections: [
    {
      key: "main",
      title: "Library",
      collapsible: false,
      collapsed_default: false,
      field_keys: ["service_kind", "season_folder", "root_folder"],
    },
  ],
};

function renderForm(
  values: Record<string, unknown>,
  extra: Partial<React.ComponentProps<typeof SchemaForm>> = {},
) {
  const onChange = vi.fn();
  render(<SchemaForm descriptor={descriptor} values={values} onChange={onChange} {...extra} />);
  return { onChange };
}

describe("SchemaForm", () => {
  it("hides a field whose show_when is unmet", () => {
    renderForm({ service_kind: "radarr" });
    expect(screen.queryByText("Season folder")).toBeNull();
  });
  it("shows a field whose show_when is met", () => {
    renderForm({ service_kind: "sonarr" });
    expect(screen.getByText("Season folder")).toBeTruthy();
  });
  it("renders dynamic options for a dynamic_options select", () => {
    renderForm({}, { dynamicOptions: { root_folder: [{ value: "/movies", label: "/movies" }] } });
    expect(screen.getByText("Root folder")).toBeTruthy();
  });
  it("renders a server field error", () => {
    renderForm({ service_kind: "radarr" }, { errors: { service_kind: "bad service" } });
    expect(screen.getByText("bad service")).toBeTruthy();
  });
  it("emits onChange when a switch toggles", () => {
    const { onChange } = renderForm({ service_kind: "sonarr", season_folder: false });
    fireEvent.click(screen.getByRole("switch"));
    expect(onChange).toHaveBeenCalled();
  });
  it("renders a declared default_value when the field is absent from values (#6)", () => {
    const d: PluginAdminForm = {
      fields: [
        {
          key: "season_folder",
          label: "Season folder",
          control: "SWITCH",
          required: false,
          secret: false,
          multiline: false,
          default_value: true,
        },
      ],
    };
    const onChange = vi.fn();
    render(<SchemaForm descriptor={d} values={{}} onChange={onChange} />);
    expect((screen.getByRole("switch") as HTMLButtonElement).getAttribute("aria-checked")).toBe(
      "true",
    );
  });
  it("uses a controlling field default when rendering a conditional field", () => {
    const d: PluginAdminForm = {
      fields: [
        {
          key: "advanced_enabled",
          label: "Advanced",
          control: "SWITCH",
          required: false,
          secret: false,
          multiline: false,
          default_value: true,
        },
        {
          key: "endpoint",
          label: "Endpoint",
          control: "TEXT",
          required: false,
          secret: false,
          multiline: false,
          show_when: [{ field: "advanced_enabled", equals: ["true"] }],
        },
      ],
    };
    render(<SchemaForm descriptor={d} values={{}} onChange={vi.fn()} />);
    expect(screen.getByText("Endpoint")).toBeTruthy();
  });
  it("reports validity through onValidityChange (#14)", () => {
    const onValidityChange = vi.fn();
    const d: PluginAdminForm = {
      fields: [
        {
          key: "name",
          label: "Name",
          control: "TEXT",
          required: true,
          secret: false,
          multiline: false,
        },
      ],
    };
    const { rerender } = render(
      <SchemaForm
        descriptor={d}
        values={{}}
        onChange={vi.fn()}
        onValidityChange={onValidityChange}
      />,
    );
    expect(onValidityChange).toHaveBeenLastCalledWith(false);
    rerender(
      <SchemaForm
        descriptor={d}
        values={{ name: "ok" }}
        onChange={vi.fn()}
        onValidityChange={onValidityChange}
      />,
    );
    expect(onValidityChange).toHaveBeenLastCalledWith(true);
  });
});

const collapsibleDescriptor: PluginAdminForm = {
  fields: [
    {
      key: "api_path",
      label: "API path",
      control: "TEXT",
      required: true,
      secret: false,
      multiline: false,
    },
    {
      key: "verbose",
      label: "Verbose",
      control: "SWITCH",
      required: false,
      secret: false,
      multiline: false,
    },
  ],
  sections: [
    {
      key: "lib",
      title: "Library",
      collapsible: true,
      collapsed_default: true,
      field_keys: ["api_path", "verbose"],
    },
  ],
};

describe("SchemaForm collapsible sections", () => {
  it("honors collapsed_default when the section has no field errors", () => {
    render(
      <SchemaForm
        descriptor={collapsibleDescriptor}
        values={{ api_path: "/v3" }}
        onChange={vi.fn()}
      />,
    );
    expect(screen.queryByText("Verbose")).toBeNull(); // collapsed -> field hidden
    expect(screen.getByText("Show")).toBeTruthy();
  });

  it("auto-expands a collapsed section that has a validation error (empty required field)", () => {
    render(<SchemaForm descriptor={collapsibleDescriptor} values={{}} onChange={vi.fn()} />);
    // api_path is required + empty -> validateSchemaValues flags it -> section force-expands
    expect(screen.getByText("Verbose")).toBeTruthy();
  });

  it("expands a clean collapsed section when Show is clicked", () => {
    render(
      <SchemaForm
        descriptor={collapsibleDescriptor}
        values={{ api_path: "/v3" }}
        onChange={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByText("Show"));
    expect(screen.getByText("Verbose")).toBeTruthy();
  });

  it("uses a controlling field default when rendering a conditional section", () => {
    const d: PluginAdminForm = {
      fields: [
        {
          key: "advanced_enabled",
          label: "Advanced",
          control: "SWITCH",
          required: false,
          secret: false,
          multiline: false,
          default_value: true,
        },
        {
          key: "endpoint",
          label: "Endpoint",
          control: "TEXT",
          required: false,
          secret: false,
          multiline: false,
        },
      ],
      sections: [
        {
          key: "advanced",
          title: "Advanced options",
          collapsible: false,
          collapsed_default: false,
          field_keys: ["endpoint"],
          show_when: [{ field: "advanced_enabled", equals: ["true"] }],
        },
      ],
    };
    render(<SchemaForm descriptor={d} values={{}} onChange={vi.fn()} />);
    expect(screen.getByText("Advanced options")).toBeTruthy();
    expect(screen.getByText("Endpoint")).toBeTruthy();
  });
});

it("marks a show_when-gated field as nested when it is revealed", () => {
  const d: PluginAdminForm = {
    fields: [
      {
        key: "service_kind",
        label: "Service",
        control: "SELECT",
        required: false,
        secret: false,
        multiline: false,
        options: [{ value: "sonarr", label: "Sonarr" }],
      },
      {
        key: "series_type",
        label: "Series type",
        control: "SELECT",
        required: false,
        secret: false,
        multiline: false,
        show_when: [{ field: "service_kind", equals: ["sonarr"] }],
        options: [{ value: "standard", label: "Standard" }],
      },
    ],
  };
  const { container } = render(
    <SchemaForm descriptor={d} values={{ service_kind: "sonarr" }} onChange={vi.fn()} />,
  );
  expect(container.querySelector('[data-nested="true"]')).not.toBeNull();
});

it("renders inputs with autocomplete and password-manager ignore attributes", () => {
  const d: PluginAdminForm = {
    fields: [
      {
        key: "manifest_url",
        label: "Manifest URL",
        control: "TEXT",
        required: false,
        secret: false,
        multiline: false,
      },
      {
        key: "api_key",
        label: "API Key",
        control: "PASSWORD",
        required: false,
        secret: true,
        multiline: false,
      },
    ],
  };
  const { container } = render(<SchemaForm descriptor={d} values={{}} onChange={vi.fn()} />);
  const textInput = container.querySelector("#schema-manifest_url");
  expect(textInput?.getAttribute("autocomplete")).toBe("off");
  expect(textInput?.getAttribute("data-1p-ignore")).toBe("true");

  const passwordInput = container.querySelector("#schema-api_key");
  expect(passwordInput?.getAttribute("autocomplete")).toBe("new-password");
  expect(passwordInput?.getAttribute("data-1p-ignore")).toBe("true");
});

describe("SchemaForm library picker", () => {
  it("requests libraries only when the descriptor has a library picker", () => {
    const { rerender } = render(
      <SchemaForm descriptor={descriptor} values={{}} onChange={vi.fn()} />,
    );
    expect(useAdminLibrariesCalls[0]).toEqual({ enabled: false });

    rerender(
      <SchemaForm descriptor={libraryPickerDescriptor("any")} values={{}} onChange={vi.fn()} />,
    );
    expect(useAdminLibrariesCalls[useAdminLibrariesCalls.length - 1]).toEqual({ enabled: true });
  });

  it("shows the selected library by name and id", () => {
    librariesState.current = LIBRARIES;
    render(
      <SchemaForm
        descriptor={libraryPickerDescriptor("any")}
        values={{ library_id: "3" }}
        onChange={vi.fn()}
      />,
    );
    expect(screen.getByText("Shows (3)")).toBeTruthy();
  });

  it("lists only movie and mixed libraries for a movie picker", async () => {
    librariesState.current = LIBRARIES;
    render(
      <SchemaForm descriptor={libraryPickerDescriptor("movie")} values={{}} onChange={vi.fn()} />,
    );
    await userEvent.click(screen.getByRole("combobox", { name: "Library" }));
    expect(await screen.findByRole("option", { name: "Movies (1)" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Mixed (5)" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "Shows (3)" })).toBeNull();
    expect(screen.queryByRole("option", { name: "Disabled movies (6)" })).toBeNull();
  });

  it("keeps a saved value with no matching library as a not-found option", () => {
    librariesState.current = LIBRARIES;
    render(
      <SchemaForm
        descriptor={libraryPickerDescriptor("movie")}
        values={{ library_id: "99" }}
        onChange={vi.fn()}
      />,
    );
    expect(screen.getByText("99 (not found)")).toBeTruthy();
  });

  it("keeps a saved value while the library list is still empty", () => {
    librariesState.current = [];
    render(
      <SchemaForm
        descriptor={libraryPickerDescriptor("any")}
        values={{ library_id: "3" }}
        onChange={vi.fn()}
      />,
    );
    expect(screen.getByText("3 (not found)")).toBeTruthy();
  });
});
