import { useEffect, useState } from "react";
import { Sliders } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

import type { QualityProfileRule } from "./scoringPresets";

interface QualityProfileModalProps {
  isOpen: boolean;
  onClose: () => void;
  onSave: (profile: QualityProfileRule) => void;
  editingProfile?: QualityProfileRule | null;
  defaultOrder?: number;
}

const DEFAULT_PROFILE: QualityProfileRule = {
  label: "",
  resolution: "1080p",
  hdr: "",
  exclude_hdr: "",
  codec_video: "",
  codec_audio: "",
  include_regex: "",
  exclude_regex: "",
  preferred_order: 1,
};

export function QualityProfileModal({
  isOpen,
  onClose,
  onSave,
  editingProfile,
  defaultOrder = 1,
}: QualityProfileModalProps) {
  const [formData, setFormData] = useState<QualityProfileRule>(DEFAULT_PROFILE);

  useEffect(() => {
    if (editingProfile) {
      setFormData({ ...editingProfile });
    } else {
      setFormData({ ...DEFAULT_PROFILE, preferred_order: defaultOrder });
    }
  }, [editingProfile, defaultOrder, isOpen]);

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!formData.label.trim()) return;
    onSave({
      ...formData,
      label: formData.label.trim(),
    });
    onClose();
  };

  return (
    <Dialog open={isOpen} onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <div className="flex items-center gap-2.5">
            <div className="bg-primary/10 text-primary flex size-9 items-center justify-center rounded-lg">
              <Sliders className="size-4" />
            </div>
            <div>
              <DialogTitle>
                {editingProfile ? "Edit Quality Profile" : "Add Quality Profile"}
              </DialogTitle>
              <DialogDescription>
                Define resolution, HDR, and codec preferences for streaming candidate selection.
              </DialogDescription>
            </div>
          </div>
        </DialogHeader>

        <form onSubmit={handleSubmit} className="space-y-4 pt-2">
          <div className="grid grid-cols-2 gap-4">
            <div className="space-y-1.5">
              <Label htmlFor="qp-label" className="text-xs font-semibold">
                Profile Label
              </Label>
              <Input
                id="qp-label"
                placeholder="e.g. 4K HDR10"
                value={formData.label}
                required
                onChange={(e) => setFormData((p) => ({ ...p, label: e.target.value }))}
              />
            </div>

            <div className="space-y-1.5">
              <Label htmlFor="qp-order" className="text-xs font-semibold">
                Rank Order (1 = Highest)
              </Label>
              <Input
                id="qp-order"
                type="number"
                min={1}
                max={100}
                value={formData.preferred_order}
                required
                onChange={(e) =>
                  setFormData((p) => ({ ...p, preferred_order: Number(e.target.value) || 1 }))
                }
              />
            </div>
          </div>

          <div className="grid grid-cols-2 gap-4">
            <div className="space-y-1.5">
              <Label htmlFor="qp-resolution" className="text-xs font-semibold">
                Target Resolution
              </Label>
              <Select
                value={formData.resolution || "any"}
                onValueChange={(val) =>
                  setFormData((p) => ({ ...p, resolution: val === "any" ? "" : val }))
                }
              >
                <SelectTrigger id="qp-resolution">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="any">Any Resolution</SelectItem>
                  <SelectItem value="2160p">2160p (4K UHD)</SelectItem>
                  <SelectItem value="1080p">1080p (Full HD)</SelectItem>
                  <SelectItem value="720p">720p (HD)</SelectItem>
                  <SelectItem value="480p">480p (SD)</SelectItem>
                </SelectContent>
              </Select>
            </div>

            <div className="space-y-1.5">
              <Label htmlFor="qp-hdr" className="text-xs font-semibold">
                Required HDR
              </Label>
              <Select
                value={formData.hdr || "any"}
                onValueChange={(val) =>
                  setFormData((p) => ({ ...p, hdr: val === "any" ? "" : val }))
                }
              >
                <SelectTrigger id="qp-hdr">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="any">Any Dynamic Range</SelectItem>
                  <SelectItem value="hdr">Any HDR (HDR10/DV/HLG)</SelectItem>
                  <SelectItem value="dv">Dolby Vision (DV)</SelectItem>
                  <SelectItem value="hdr10">HDR10 / HDR10+</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="grid grid-cols-2 gap-4">
            <div className="space-y-1.5">
              <Label htmlFor="qp-vcodec" className="text-xs font-semibold">
                Video Codec (optional)
              </Label>
              <Input
                id="qp-vcodec"
                placeholder="e.g. hevc, h264, av1"
                value={formData.codec_video || ""}
                onChange={(e) => setFormData((p) => ({ ...p, codec_video: e.target.value }))}
              />
            </div>

            <div className="space-y-1.5">
              <Label htmlFor="qp-acodec" className="text-xs font-semibold">
                Audio Codec (optional)
              </Label>
              <Input
                id="qp-acodec"
                placeholder="e.g. truehd, dts, eac3, aac"
                value={formData.codec_audio || ""}
                onChange={(e) => setFormData((p) => ({ ...p, codec_audio: e.target.value }))}
              />
            </div>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="qp-inregex" className="text-xs font-semibold">
              Include Pattern Regex (optional)
            </Label>
            <Input
              id="qp-inregex"
              className="font-mono text-xs"
              placeholder="(?i)(remux|web-dl)"
              value={formData.include_regex || ""}
              onChange={(e) => setFormData((p) => ({ ...p, include_regex: e.target.value }))}
            />
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="qp-exregex" className="text-xs font-semibold">
              Exclude Pattern Regex (optional)
            </Label>
            <Input
              id="qp-exregex"
              className="font-mono text-xs"
              placeholder="(?i)(cam|telesync|sample)"
              value={formData.exclude_regex || ""}
              onChange={(e) => setFormData((p) => ({ ...p, exclude_regex: e.target.value }))}
            />
          </div>

          <DialogFooter className="pt-2">
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={!formData.label.trim()}>
              Save Profile
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
