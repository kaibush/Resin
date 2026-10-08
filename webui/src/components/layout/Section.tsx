import type { HTMLAttributes, ReactNode } from "react";
import { cn } from "../../lib/cn";
import { Card } from "../ui/Card";

type SectionHeaderProps = {
  title?: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
};

export function SectionHeader({ title, description, actions }: SectionHeaderProps) {
  if (title == null && description == null && actions == null) {
    return null;
  }
  return (
    <div className="list-card-header surface-header">
      <div className="surface-copy">
        {title != null ? <h3>{title}</h3> : null}
        {description != null ? <p>{description}</p> : null}
      </div>
      {actions}
    </div>
  );
}

type SectionProps = HTMLAttributes<HTMLDivElement> & SectionHeaderProps;

export function Section({ title, description, actions, className, children, ...props }: SectionProps) {
  return (
    <Card className={cn("surface", className)} {...props}>
      <SectionHeader title={title} description={description} actions={actions} />
      {children}
    </Card>
  );
}

export function Surface({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <Card className={cn("surface", className)} {...props} />;
}
