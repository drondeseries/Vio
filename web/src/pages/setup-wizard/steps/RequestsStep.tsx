import { useRequestIntegrations } from "@/hooks/queries/useRequests";
import { RequestIntegrationsTab } from "@/pages/AdminRequests";

import { StepFrame, StepSection } from "../StepFrame";
import { useStepSummary } from "../useStep";
import { useWizardContext } from "../WizardContext";

export function RequestsStep() {
  const { markDone } = useWizardContext();
  const { data: integrations = [] } = useRequestIntegrations();

  const enabledCount = integrations.filter((i) => i.enabled).length;
  const summary =
    integrations.length > 0
      ? `${enabledCount} active integration${enabledCount === 1 ? "" : "s"}`
      : "Skipped";
  useStepSummary("requests", summary);

  return (
    <StepFrame
      title="Media Requests"
      lede="Connect automated media fulfillment services (like Radarr, Sonarr, or AltMount) so users can request movies and series directly within Vio."
      onContinue={() => markDone("requests")}
      continueLabel="Continue"
      onSkip={() => markDone("requests")}
      footnote="Integrations can be added or updated anytime in Admin › Requests."
    >
      <StepSection
        title="Fulfillment integrations"
        caption="Configure request router plugins and downloader connections to automatically fulfill user requests."
      >
        <div className="pt-2">
          <RequestIntegrationsTab />
        </div>
      </StepSection>
    </StepFrame>
  );
}
