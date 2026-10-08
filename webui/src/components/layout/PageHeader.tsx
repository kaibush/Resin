import type { ReactNode } from "react";
import { cn } from "../../lib/cn";

type PageHeaderProps = {
  title: ReactNode;
  description?: ReactNode;
  meta?: ReactNode;
  actions?: ReactNode;
  className?: string;
};

export function PageHeader({ title, description, meta, actions, className }: PageHeaderProps) {
  return (
    <header className={cn("module-header page-header", className)}>
      <div className="page-header-copy">
        <h2>{title}</h2>
        {description ? <p className="module-description">{description}</p> : null}
        {meta}
      </div>
      {actions ? <div className="page-header-actions">{actions}</div> : null}
    </header>
  );
}
