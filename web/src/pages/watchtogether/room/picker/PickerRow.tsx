import { useState } from "react";
import { ChevronRight } from "lucide-react";
import MediaCarousel from "@/components/MediaCarousel";
import { decodeThumbhash } from "@/lib/thumbhash";
import type { PickerEntry } from "@/api/v2/watchTogetherPicker";
import type { WatchTogetherRoomMember } from "@/lib/watchTogether";
import { MemberAvatar } from "../MemberAvatar";
import { memberTints, type MemberTint } from "../members";

/** The subset of a catalog card the picker needs; both search and picker rows fit it. */
export interface PickerCard {
  content_id: string;
  type: string;
  title: string;
  year?: number;
  poster_url?: string;
  poster_thumbhash?: string;
  library_id?: number;
}

export function PosterTile({
  card,
  caption,
  who,
  tints,
  progress,
  onClick,
  selected,
}: {
  card: PickerCard;
  caption?: string;
  who?: { user_id: number; profile_id: string; display_name: string }[];
  tints?: Map<string, MemberTint>;
  progress?: number;
  onClick: () => void;
  selected?: boolean;
}) {
  const [loaded, setLoaded] = useState(false);
  const thumbhash = card.poster_thumbhash ? decodeThumbhash(card.poster_thumbhash) : "";
  return (
    <button
      type="button"
      onClick={onClick}
      className="group/tile w-28 shrink-0 text-left sm:w-32"
      aria-label={card.title}
    >
      <div
        className={`media-card-image relative aspect-[2/3] transition-all ${
          selected ? "ring-primary ring-offset-background ring-2 ring-offset-2" : ""
        }`}
        style={
          thumbhash ? { backgroundImage: `url(${thumbhash})`, backgroundSize: "cover" } : undefined
        }
      >
        {card.poster_url ? (
          <img
            src={card.poster_url}
            alt=""
            loading="lazy"
            onLoad={() => setLoaded(true)}
            className={`h-full w-full object-cover transition-opacity duration-300 ${loaded ? "opacity-100" : "opacity-0"}`}
          />
        ) : (
          <div className="text-muted-foreground flex h-full items-center justify-center p-2 text-center text-xs">
            {card.title}
          </div>
        )}
        {card.type === "series" ? (
          <span className="glass-subtle absolute right-1.5 bottom-1.5 flex items-center gap-0.5 rounded-full border border-white/15 px-1.5 py-0.5 text-[10px] font-semibold text-white/90">
            Series <ChevronRight className="size-2.5" />
          </span>
        ) : null}
        {who && who.length > 0 ? (
          <span className="absolute top-1.5 left-1.5 flex -space-x-1.5">
            {who.slice(0, 4).map((m) => (
              <MemberAvatar
                key={`${m.user_id}:${m.profile_id}`}
                name={m.display_name}
                tint={tints?.get(`${m.user_id}:${m.profile_id}`)}
                size="sm"
                className="ring-background size-5 ring-1"
              />
            ))}
          </span>
        ) : null}
        {progress !== undefined ? (
          <span className="absolute inset-x-0 bottom-0 h-1 bg-black/40">
            <span
              className="bg-foreground block h-full"
              style={{ width: `${Math.round(progress * 100)}%` }}
            />
          </span>
        ) : null}
      </div>
      <div className="px-0.5 pt-2">
        <div className="truncate text-[13px] font-semibold">{card.title}</div>
        <div className="text-muted-foreground mt-0.5 truncate text-[11px]">
          {caption ?? (card.year ? String(card.year) : card.type === "series" ? "Series" : "Movie")}
        </div>
      </div>
    </button>
  );
}

function pickerCaption(entry: PickerEntry) {
  const names = entry.members.map((m) => m.display_name);
  if (entry.next_up) {
    return `S${entry.next_up.season_number} E${entry.next_up.episode_number} · ${names.join(", ")}`;
  }
  return names.join(", ");
}

function groupProgress(entry: PickerEntry) {
  const ratios = entry.members
    .filter((m) => m.position_seconds !== undefined && m.duration_seconds)
    .map((m) => m.position_seconds! / m.duration_seconds!);
  if (ratios.length === 0) return undefined;
  return Math.min(...ratios);
}

/** One picker row: the shared carousel in its compact form, with an optional hint. */
export function PickerRowSection({
  title,
  hint,
  loading,
  children,
}: {
  title: string;
  hint?: string;
  loading?: boolean;
  children: React.ReactNode;
}) {
  return (
    <MediaCarousel
      title={title}
      compact
      edgePadding={false}
      loading={loading}
      skeletonCount={5}
      headerActions={
        hint ? <span className="text-muted-foreground text-[11px]">· {hint}</span> : undefined
      }
    >
      {children}
    </MediaCarousel>
  );
}

export function TogetherRow({
  title,
  hint,
  entries,
  members,
  selectedId,
  onSelect,
}: {
  title: string;
  hint?: string;
  entries: PickerEntry[];
  members: WatchTogetherRoomMember[];
  selectedId?: string;
  onSelect: (entry: PickerEntry) => void;
}) {
  if (entries.length === 0) return null;
  const tints = memberTints(members);
  return (
    <PickerRowSection title={title} hint={hint}>
      {entries.map((entry) => (
        <PosterTile
          key={entry.item.content_id}
          card={entry.item}
          caption={pickerCaption(entry)}
          who={entry.members}
          tints={tints}
          progress={groupProgress(entry)}
          selected={selectedId === entry.item.content_id}
          onClick={() => onSelect(entry)}
        />
      ))}
    </PickerRowSection>
  );
}
