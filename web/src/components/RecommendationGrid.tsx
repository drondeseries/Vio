import ViewTransitionLink from "@/components/ViewTransitionLink";
import MediaCarousel from "@/components/MediaCarousel";
import { useCatalogItemDetail } from "@/hooks/queries/catalogRead";
import { isTerminalNotFoundError } from "@/hooks/queries/mediaSurfaceRefresh";
import { useUICustomization } from "@/hooks/useUICustomization";
import { carouselCardWidthClasses } from "@/lib/uiCustomization";
import CardPlayOverlay from "@/components/CardPlayOverlay";
import { useCallback, useEffect, useState } from "react";

const MAX_MORE_LIKE_THIS_ITEMS = 12;

interface RecommendationGridProps {
  items: Array<{ content_id: string }>;
  maxItems?: number;
}

interface RecommendationItemCardProps {
  itemId: string;
  showCaption: boolean;
  className: string;
  onTerminalError: (itemId: string) => void;
}

function RecommendationItemCard({
  itemId,
  showCaption,
  className,
  onTerminalError,
}: RecommendationItemCardProps) {
  const { data: item, error } = useCatalogItemDetail(itemId);
  // A 404 is terminal: the recommendation points at an item the catalog no
  // longer has. Report it so the grid unmounts this card, which lets the
  // query deactivate instead of polling a dead endpoint forever.
  const terminal = isTerminalNotFoundError(error);

  useEffect(() => {
    if (terminal) onTerminalError(itemId);
  }, [terminal, itemId, onTerminalError]);

  if (terminal) {
    return null;
  }

  if (!item) {
    return (
      <div className={className}>
        <div className="bg-surface aspect-[2/3] animate-pulse rounded-lg" />
      </div>
    );
  }

  return (
    <div className={className}>
      <div className="group/card">
        <div className="group/media relative">
          <ViewTransitionLink to={`/item/${encodeURIComponent(itemId)}`} className="group block">
            <div className="aspect-[2/3] overflow-hidden rounded-lg">
              {item.poster_url ? (
                <img
                  src={item.poster_url}
                  alt={item.title}
                  loading="lazy"
                  decoding="async"
                  className="h-full w-full object-cover transition-transform group-hover:scale-105"
                />
              ) : (
                <div className="bg-surface text-muted-foreground flex h-full items-center justify-center text-xs">
                  {item.title}
                </div>
              )}
            </div>
          </ViewTransitionLink>
          {item.play_content_id ? (
            <CardPlayOverlay
              contentId={item.play_content_id}
              title={item.title}
              type={item.type === "movie" ? "movie" : "episode"}
            />
          ) : null}
        </div>
        {showCaption ? (
          <ViewTransitionLink
            to={`/item/${encodeURIComponent(itemId)}`}
            className="mt-1.5 block truncate text-sm font-medium hover:underline"
          >
            {item.title}
          </ViewTransitionLink>
        ) : null}
      </div>
    </div>
  );
}

export default function RecommendationGrid({ items, maxItems = 12 }: RecommendationGridProps) {
  const { cardPresentation } = useUICustomization();
  const itemLimit = Math.max(0, Math.min(maxItems, MAX_MORE_LIKE_THIS_ITEMS));
  const posterWidthClasses = carouselCardWidthClasses(cardPresentation.poster_size);
  const [failedItemIds, setFailedItemIds] = useState<ReadonlySet<string>>(() => new Set());

  const handleTerminalError = useCallback((itemId: string) => {
    setFailedItemIds((previous) => {
      if (previous.has(itemId)) return previous;
      const next = new Set(previous);
      next.add(itemId);
      return next;
    });
  }, []);

  // Drop terminally-failed ids before applying the limit so a removed card is
  // replaced by the next healthy recommendation rather than leaving a gap.
  const visibleItems = items.filter((si) => !failedItemIds.has(si.content_id));

  return (
    <MediaCarousel title="More Like This" edgePadding={false}>
      {visibleItems.slice(0, itemLimit).map((si) => (
        <RecommendationItemCard
          key={si.content_id}
          itemId={si.content_id}
          showCaption={cardPresentation.caption !== "artwork"}
          className={posterWidthClasses}
          onTerminalError={handleTerminalError}
        />
      ))}
    </MediaCarousel>
  );
}
